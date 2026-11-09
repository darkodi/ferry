// Package destination stores customers' payout destinations: the bank
// accounts fiat is paid to.
//
// The bank details are the rail's payment form (for Noah, the channel's
// form fields). They are personal data, so the store encrypts them with
// AES-256-GCM before they reach the database and keeps only a masked
// summary ("BankAch ****5678") in clear. The associated data binds each
// ciphertext to its row id, so a ciphertext copied onto another row does
// not decrypt.
//
// Only the API process needs the data key (it quotes payouts). The worker
// opens the store without a key: it never reads bank details, it only
// records the rail's saved payment method id.
package destination

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darkodi/ferry/internal/destination/destinationdb"
	"github.com/darkodi/ferry/internal/rail"
)

// KeySize is the data key length: AES-256.
const KeySize = 32

// Form limits. Rail forms are a few short fields; anything larger is not
// a bank account.
const (
	maxFormBytes  = 8 << 10
	maxFormDepth  = 4
	maxFormFields = 50
	maxValueLen   = 256
)

// Errors. Match them with errors.Is.
var (
	ErrInvalid  = errors.New("destination: invalid")
	ErrNotFound = errors.New("destination: not found")
	ErrNoKey    = errors.New("destination: no data key configured")
)

// Destination is a saved bank account, without its bank details.
type Destination struct {
	ID                  uuid.UUID
	CustomerID          uuid.UUID
	Rail                string
	Country             string
	FiatCurrency        string
	PaymentMethodType   string
	Display             string
	RailPaymentMethodID string
	Status              string
	CreatedAt           time.Time
}

// CreateRequest is a customer registering a bank account.
type CreateRequest struct {
	CustomerID        uuid.UUID
	Country           string
	FiatCurrency      string
	PaymentMethodType string
	Form              json.RawMessage
}

// Store reads and writes destinations. It satisfies payout.Destinations.
type Store struct {
	db    destinationdb.DBTX
	rail  string
	aead  cipher.AEAD // nil without a key
	keyID string
}

// NewStore returns a store for destinations of the named rail. key is
// the 32-byte data key, or nil for a process that never reads bank
// details (Create and ForQuote then fail with ErrNoKey).
func NewStore(db destinationdb.DBTX, railName string, key []byte) (*Store, error) {
	s := &Store{db: db, rail: railName}
	if key == nil {
		return s, nil
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("destination: data key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("destination: data key: %w", err)
	}
	if s.aead, err = cipher.NewGCM(block); err != nil {
		return nil, fmt.Errorf("destination: data key: %w", err)
	}
	// A short fingerprint names the key without revealing it, so rows can
	// be re-encrypted when the key rotates (KMS, M7).
	sum := sha256.Sum256(key)
	s.keyID = hex.EncodeToString(sum[:4])
	return s, nil
}

var (
	countryRE  = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)
	methodRE   = regexp.MustCompile(`^[A-Za-z]{2,40}$`)
)

// Create validates, encrypts and stores a destination.
func (s *Store) Create(ctx context.Context, req CreateRequest) (Destination, error) {
	if s.aead == nil {
		return Destination{}, ErrNoKey
	}
	switch {
	case req.CustomerID == uuid.Nil:
		return Destination{}, fmt.Errorf("%w: customer id required", ErrInvalid)
	case !countryRE.MatchString(req.Country):
		return Destination{}, fmt.Errorf("%w: country must be two capital letters", ErrInvalid)
	case !currencyRE.MatchString(req.FiatCurrency):
		return Destination{}, fmt.Errorf("%w: fiat currency must be three capital letters", ErrInvalid)
	case !methodRE.MatchString(req.PaymentMethodType):
		return Destination{}, fmt.Errorf("%w: payment method type must be 2-40 letters", ErrInvalid)
	}
	form, err := CheckForm(req.Form)
	if err != nil {
		return Destination{}, err
	}
	id := uuid.New()
	row, err := destinationdb.New(s.db).InsertDestination(ctx, destinationdb.InsertDestinationParams{
		ID:                id,
		CustomerID:        req.CustomerID,
		Rail:              s.rail,
		Country:           req.Country,
		FiatCurrency:      req.FiatCurrency,
		PaymentMethodType: req.PaymentMethodType,
		Display:           Display(req.PaymentMethodType, form),
		FormCiphertext:    s.seal(id, form),
		KeyID:             s.keyID,
	})
	if err != nil {
		return Destination{}, fmt.Errorf("destination: insert: %w", err)
	}
	return fromRow(row), nil
}

// Get returns a customer's destination. Another customer's is reported
// as ErrNotFound, so ids cannot be probed.
func (s *Store) Get(ctx context.Context, customerID, id uuid.UUID) (Destination, error) {
	row, err := s.row(ctx, customerID, id)
	if err != nil {
		return Destination{}, err
	}
	return fromRow(row), nil
}

// List returns a customer's destinations, newest first.
func (s *Store) List(ctx context.Context, customerID uuid.UUID) ([]Destination, error) {
	rows, err := destinationdb.New(s.db).CustomerDestinations(ctx, destinationdb.CustomerDestinationsParams{
		CustomerID: customerID, MaxRows: 100,
	})
	if err != nil {
		return nil, fmt.Errorf("destination: list: %w", err)
	}
	out := make([]Destination, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, nil
}

// Check reports whether the customer may pay out to destination id in
// currency: it exists, is theirs, is active, uses this store's rail and
// pays out in that currency.
func (s *Store) Check(ctx context.Context, customerID, id uuid.UUID, currency string) error {
	d, err := s.Get(ctx, customerID, id)
	if err != nil {
		return err
	}
	switch {
	case d.Status != "active":
		return fmt.Errorf("%w: destination is %s", ErrInvalid, d.Status)
	case d.Rail != s.rail:
		return fmt.Errorf("%w: destination was registered for the %s rail", ErrInvalid, d.Rail)
	case d.FiatCurrency != currency:
		return fmt.Errorf("%w: destination pays out in %s, not %s", ErrInvalid, d.FiatCurrency, currency)
	}
	return nil
}

// ForQuote returns the destination with its decrypted bank details, for
// the rail's quote. The result must not be logged.
func (s *Store) ForQuote(ctx context.Context, customerID, id uuid.UUID) (rail.Destination, error) {
	if s.aead == nil {
		return rail.Destination{}, ErrNoKey
	}
	row, err := s.row(ctx, customerID, id)
	if err != nil {
		return rail.Destination{}, err
	}
	form, err := s.open(row.ID, row.FormCiphertext)
	if err != nil {
		return rail.Destination{}, err
	}
	d := rail.Destination{
		ID: row.ID, Country: row.Country, FiatCurrency: row.FiatCurrency,
		PaymentMethodType: row.PaymentMethodType, Form: form,
	}
	if row.RailPaymentMethodID != nil {
		d.PaymentMethodID = *row.RailPaymentMethodID
	}
	return d, nil
}

// RememberPaymentMethod records the rail's saved payment method for a
// destination, in the caller's transaction (the payout's settle).
func (s *Store) RememberPaymentMethod(ctx context.Context, tx pgx.Tx, id uuid.UUID, paymentMethodID string) error {
	if paymentMethodID == "" || len(paymentMethodID) > 150 {
		return nil // nothing usable to remember
	}
	err := destinationdb.New(tx).SetRailPaymentMethod(ctx, destinationdb.SetRailPaymentMethodParams{
		ID: id, RailPaymentMethodID: &paymentMethodID,
	})
	if err != nil {
		return fmt.Errorf("destination: remember payment method: %w", err)
	}
	return nil
}

func (s *Store) row(ctx context.Context, customerID, id uuid.UUID) (destinationdb.Destination, error) {
	row, err := destinationdb.New(s.db).GetDestination(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.CustomerID != customerID) {
		return destinationdb.Destination{}, ErrNotFound
	}
	if err != nil {
		return destinationdb.Destination{}, fmt.Errorf("destination: load: %w", err)
	}
	return row, nil
}

// aad binds a ciphertext to its row.
func aad(id uuid.UUID) []byte { return []byte("ferry:destination:v1:" + id.String()) }

func (s *Store) seal(id uuid.UUID, plain []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(fmt.Sprintf("destination: random nonce: %v", err)) // crypto/rand does not fail on supported platforms
	}
	return s.aead.Seal(nonce, nonce, plain, aad(id))
}

func (s *Store) open(id uuid.UUID, sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n+s.aead.Overhead() {
		return nil, fmt.Errorf("destination %s: ciphertext too short", id)
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad(id))
	if err != nil {
		// Wrong key, or the row was tampered with. Never fall back.
		return nil, fmt.Errorf("destination %s: decrypt bank details: %w", id, err)
	}
	return plain, nil
}

func fromRow(r destinationdb.Destination) Destination {
	d := Destination{
		ID: r.ID, CustomerID: r.CustomerID, Rail: r.Rail, Country: r.Country,
		FiatCurrency: r.FiatCurrency, PaymentMethodType: r.PaymentMethodType,
		Display: r.Display, Status: r.Status, CreatedAt: r.CreatedAt,
	}
	if r.RailPaymentMethodID != nil {
		d.RailPaymentMethodID = *r.RailPaymentMethodID
	}
	return d
}

// CheckForm validates bank details and returns them compacted. The form
// must be a JSON object of at most 50 fields, nested at most 4 deep, whose
// values are printable strings of at most 256 characters, booleans or
// nested objects. Field-level rules (lengths, required fields) belong to
// the rail's schema, which the rail checks when it quotes.
func CheckForm(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maxFormBytes {
		return nil, fmt.Errorf("%w: form must be 1-%d bytes", ErrInvalid, maxFormBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: form is not JSON: %w", ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data after the form", ErrInvalid)
	}
	obj, ok := v.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil, fmt.Errorf("%w: form must be a non-empty JSON object", ErrInvalid)
	}
	fields := 0
	if err := checkValue(obj, 1, &fields); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("%w: form: %w", ErrInvalid, err)
	}
	return buf.Bytes(), nil
}

func checkValue(v any, depth int, fields *int) error {
	switch x := v.(type) {
	case map[string]any:
		if depth > maxFormDepth {
			return fmt.Errorf("%w: form nested deeper than %d", ErrInvalid, maxFormDepth)
		}
		for k, child := range x {
			*fields++
			if *fields > maxFormFields {
				return fmt.Errorf("%w: form has more than %d fields", ErrInvalid, maxFormFields)
			}
			if !printableASCII(k) || len(k) == 0 || len(k) > 64 {
				return fmt.Errorf("%w: form field name %q", ErrInvalid, k)
			}
			if err := checkValue(child, depth+1, fields); err != nil {
				return err
			}
		}
	case string:
		if len(x) > maxValueLen || !printable(x) {
			return fmt.Errorf("%w: form values must be printable text of at most %d bytes", ErrInvalid, maxValueLen)
		}
	case bool:
	default:
		return fmt.Errorf("%w: form values must be text, true/false or objects", ErrInvalid)
	}
	return nil
}

func printable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// accountFields are form field names that hold an account identifier;
// Display shows only their last four characters.
var accountFields = []string{"accountnumber", "iban", "accountid", "cardnumber", "identifier"}

// Display is a masked summary of a destination, e.g. "BankAch ****5678".
// It reveals at most the last four characters of the first account
// identifier found in the form.
func Display(method string, form []byte) string {
	var obj map[string]any
	if json.Unmarshal(form, &obj) == nil {
		if acct := findAccount(obj, 1); acct != "" {
			if len(acct) > 4 {
				acct = acct[len(acct)-4:]
			}
			return method + " ****" + acct
		}
	}
	return method
}

func findAccount(obj map[string]any, depth int) string {
	if depth > maxFormDepth {
		return ""
	}
	keys := slices.Sorted(maps.Keys(obj)) // deterministic when several fields match
	for _, k := range keys {
		if s, ok := obj[k].(string); ok && isAccountField(k) {
			return strings.Map(func(r rune) rune {
				if r < 0x20 || r > 0x7e || r == ' ' {
					return -1
				}
				return r
			}, s)
		}
	}
	for _, k := range keys {
		if m, ok := obj[k].(map[string]any); ok {
			if s := findAccount(m, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func isAccountField(k string) bool {
	k = strings.ToLower(k)
	for _, f := range accountFields {
		if k == f {
			return true
		}
	}
	return false
}

// ParseKey reads the data key from its base64 form (FERRY_DATA_KEY,
// e.g. from `openssl rand -base64 32`).
func ParseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("destination: data key is not base64: %w", err)
	}
	if len(k) != KeySize {
		return nil, fmt.Errorf("destination: data key must be %d bytes, got %d", KeySize, len(k))
	}
	return k, nil
}
