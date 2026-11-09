// Package customer creates customers and authenticates their API keys.
package customer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/customer/customerdb"
	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/platform/db"
)

// KeyPrefix starts every API key, so leaked keys are easy to recognise in
// logs and secret scanners.
const KeyPrefix = "ferry_sk_"

// Errors.
var (
	ErrInvalidName  = errors.New("customer: name must be 1-200 characters")
	ErrUnauthorized = errors.New("customer: invalid API key")
	ErrNotFound     = errors.New("customer: not found")
)

// Customer is a business that deposits USDC and requests payouts.
type Customer struct {
	ID        uuid.UUID
	Name      string
	Accounts  ledger.CustomerAccounts
	CreatedAt time.Time
}

// Hook runs inside the customer's creation transaction, e.g. to assign
// deposit addresses. An error rolls the whole creation back.
type Hook func(ctx context.Context, tx pgx.Tx, customerID uuid.UUID) error

// Create adds a customer, opens their ledger accounts, issues an API key
// and runs hooks, all in one transaction. The returned key is shown once;
// only its hash is stored.
func Create(ctx context.Context, pool *pgxpool.Pool, name string, hooks ...Hook) (Customer, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return Customer{}, "", ErrInvalidName
	}
	key, err := newKey()
	if err != nil {
		return Customer{}, "", err
	}
	c := Customer{ID: uuid.New(), Name: name}
	err = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := customerdb.New(tx)
		if err := q.InsertCustomer(ctx, customerdb.InsertCustomerParams{ID: c.ID, Name: c.Name}); err != nil {
			return fmt.Errorf("customer: insert: %w", err)
		}
		accts, err := ledger.OpenCustomerAccounts(ctx, tx, c.ID)
		if err != nil {
			return fmt.Errorf("customer: open accounts: %w", err)
		}
		c.Accounts = accts
		if err := q.InsertAPIKey(ctx, customerdb.InsertAPIKeyParams{CustomerID: c.ID, KeyHash: HashKey(key)}); err != nil {
			return fmt.Errorf("customer: insert api key: %w", err)
		}
		for _, h := range hooks {
			if err := h(ctx, tx, c.ID); err != nil {
				return err
			}
		}
		row, err := q.GetCustomer(ctx, c.ID)
		if err != nil {
			return fmt.Errorf("customer: reload: %w", err)
		}
		c.CreatedAt = row.CreatedAt
		return nil
	})
	if err != nil {
		return Customer{}, "", fmt.Errorf("customer: create: %w", err)
	}
	return c, key, nil
}

// Authenticate returns the customer that owns an active API key.
func Authenticate(ctx context.Context, d ledger.DB, key string) (uuid.UUID, error) {
	if !strings.HasPrefix(key, KeyPrefix) || len(key) > 100 {
		return uuid.Nil, ErrUnauthorized
	}
	id, err := customerdb.New(d).CustomerByKeyHash(ctx, HashKey(key))
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrUnauthorized
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("customer: authenticate: %w", err)
	}
	return id, nil
}

// Get returns a customer and their account ids.
func Get(ctx context.Context, d ledger.DB, id uuid.UUID) (Customer, error) {
	row, err := customerdb.New(d).GetCustomer(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Customer{}, ErrNotFound
	}
	if err != nil {
		return Customer{}, fmt.Errorf("customer: get: %w", err)
	}
	accts, err := ledger.FindCustomerAccounts(ctx, d, id)
	if err != nil {
		return Customer{}, fmt.Errorf("customer: get accounts: %w", err)
	}
	return Customer{ID: row.ID, Name: row.Name, Accounts: accts, CreatedAt: row.CreatedAt}, nil
}

// HashKey is how API keys are stored and looked up. Keys are 32 random
// bytes, so an unsalted SHA-256 is safe: there is no dictionary to try.
func HashKey(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

func newKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("customer: generate key: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}
