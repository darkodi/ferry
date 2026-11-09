package payout

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/darkodi/ferry/internal/ledger"
	"github.com/darkodi/ferry/internal/outbox"
	"github.com/darkodi/ferry/internal/payout/payoutdb"
	"github.com/darkodi/ferry/internal/platform/db"
	"github.com/darkodi/ferry/internal/rail"
)

// Errors returned by the service. Match them with errors.Is.
var (
	ErrInvalidRequest    = errors.New("payout: invalid request")
	ErrNotFound          = errors.New("payout: not found")
	ErrKeyReused         = errors.New("payout: idempotency key reused with a different request")
	ErrNotConfirmable    = errors.New("payout: not in a confirmable state")
	ErrQuoteExpired      = errors.New("payout: quote expired")
	ErrInvalidTransition = errors.New("payout: invalid transition")
	// ErrRailMismatch: the rail reported something that cannot be true for
	// this payout (another payout's transaction, a settlement above the
	// hold). Nothing is applied; this needs a human.
	ErrRailMismatch = errors.New("payout: rail report does not match payout")
)

// errLost aborts a step whose payout or rail transaction was changed by
// someone else first. The step's transaction rolls back and the caller
// gets the current state, without an error.
var errLost = errors.New("payout: lost race")

// Payout is a request to pay fiat to a bank account from a customer's USDC.
type Payout struct {
	ID             uuid.UUID
	CustomerID     uuid.UUID
	IdempotencyKey string
	FiatAmount     int64 // minor units
	FiatCurrency   string
	// DestinationID is the saved bank account (internal/destination).
	// Payouts made before destinations existed have uuid.Nil and keep
	// their free-text Destination.
	DestinationID   uuid.UUID
	Destination     string
	Status          State
	NeedsResolution bool
	FailureReason   string
	Version         int32
	Quote           *Quote // nil until quoted
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Quote is the stored rail price for a payout.
type Quote struct {
	ID               uuid.UUID
	Rail             string
	RailQuoteID      string
	FiatAmount       int64
	FiatCurrency     string
	CryptoAuthorized ledger.Amount
	ExpiresAt        time.Time
}

// CreateRequest is what a customer asks for.
type CreateRequest struct {
	CustomerID     uuid.UUID
	IdempotencyKey string
	FiatAmount     int64 // minor units
	FiatCurrency   string
	DestinationID  uuid.UUID
}

// Destinations is where payouts find their bank accounts
// (destination.Store).
type Destinations interface {
	// Check returns nil if the customer may pay out to destination id in
	// currency, and otherwise an error saying why (Create returns it
	// unchanged).
	Check(ctx context.Context, customerID, id uuid.UUID, currency string) error
	// ForQuote returns the destination with its bank details for the
	// rail's quote.
	ForQuote(ctx context.Context, customerID, id uuid.UUID) (rail.Destination, error)
	// RememberPaymentMethod stores the rail's saved payment method for a
	// destination, inside the caller's transaction.
	RememberPaymentMethod(ctx context.Context, tx pgx.Tx, id uuid.UUID, paymentMethodID string) error
}

// Service runs payouts. Each step is one database transaction that changes
// the payout's state, posts its ledger entry and writes an outbox event
// together; rail calls happen only between those transactions.
type Service struct {
	pool  *pgxpool.Pool
	rail  rail.Rail
	dests Destinations
	house ledger.HouseAccounts
	now   func() time.Time
}

// NewService returns a Service. now is the clock used for quote expiry.
func NewService(pool *pgxpool.Pool, r rail.Rail, dests Destinations, house ledger.HouseAccounts, now func() time.Time) *Service {
	return &Service{pool: pool, rail: r, dests: dests, house: house, now: now}
}

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

func (r CreateRequest) validate() error {
	switch {
	case r.CustomerID == uuid.Nil:
		return fmt.Errorf("%w: customer id required", ErrInvalidRequest)
	case len(r.IdempotencyKey) < 1 || len(r.IdempotencyKey) > 200:
		return fmt.Errorf("%w: idempotency key must be 1-200 bytes", ErrInvalidRequest)
	case r.FiatAmount <= 0:
		return fmt.Errorf("%w: fiat amount must be positive", ErrInvalidRequest)
	case !currencyRE.MatchString(r.FiatCurrency):
		return fmt.Errorf("%w: fiat currency must be three capital letters", ErrInvalidRequest)
	case r.DestinationID == uuid.Nil:
		return fmt.Errorf("%w: destination_id required", ErrInvalidRequest)
	}
	return nil
}

// Create records a payout and quotes it. Calling it again with the same
// customer and idempotency key returns the same payout (resuming the quote
// if an earlier call stopped before it); a different request under that key
// fails with ErrKeyReused.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Payout, error) {
	if err := req.validate(); err != nil {
		return Payout{}, err
	}
	if err := s.dests.Check(ctx, req.CustomerID, req.DestinationID, req.FiatCurrency); err != nil {
		return Payout{}, err
	}
	var id uuid.UUID
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := payoutdb.New(tx)
		got, err := q.InsertPayout(ctx, payoutdb.InsertPayoutParams{
			ID:             uuid.New(),
			CustomerID:     req.CustomerID,
			IdempotencyKey: req.IdempotencyKey,
			FiatAmount:     req.FiatAmount,
			FiatCurrency:   req.FiatCurrency,
			Destination:    req.DestinationID.String(),
			DestinationID:  &req.DestinationID,
		})
		if errors.Is(err, pgx.ErrNoRows) { // key already used: return that payout
			id, err = q.PayoutByKey(ctx, payoutdb.PayoutByKeyParams{
				CustomerID: req.CustomerID, IdempotencyKey: req.IdempotencyKey,
			})
			if err != nil {
				return fmt.Errorf("payout: find by key: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("payout: insert: %w", err)
		}
		id = got
		return outbox.Write(ctx, tx, "payout.created", id, event{PayoutID: id, To: Created})
	})
	if err != nil {
		return Payout{}, err
	}
	p, err := s.load(ctx, id)
	if err != nil {
		return Payout{}, err
	}
	if p.FiatAmount != req.FiatAmount || p.FiatCurrency != req.FiatCurrency || p.DestinationID != req.DestinationID {
		return Payout{}, ErrKeyReused
	}
	if p.Status != Created {
		return p, nil
	}
	return s.quote(ctx, p)
}

// quote asks the rail for a price and stores it (created -> quoted). If the
// rail refuses, the payout fails; on any other error it stays created and a
// retried Create resumes here.
func (s *Service) quote(ctx context.Context, p Payout) (Payout, error) {
	if p.DestinationID == uuid.Nil {
		return s.step(ctx, p, Failed, "no saved destination", nil)
	}
	dest, err := s.dests.ForQuote(ctx, p.CustomerID, p.DestinationID)
	if err != nil {
		return p, fmt.Errorf("payout: destination for quote: %w", err)
	}
	rq, err := s.rail.Prepare(ctx, rail.QuoteRequest{
		PayoutID:     p.ID,
		FiatAmount:   p.FiatAmount,
		FiatCurrency: p.FiatCurrency,
		Destination:  dest,
	})
	if errors.Is(err, rail.ErrRejected) {
		return s.step(ctx, p, Failed, "quote refused: "+err.Error(), nil)
	}
	if err != nil {
		return p, fmt.Errorf("payout: quote: %w", err)
	}
	if rq.CryptoAuthorized <= 0 {
		return p, fmt.Errorf("%w: quote authorizes %s", ErrRailMismatch, rq.CryptoAuthorized)
	}
	return s.step(ctx, p, Quoted, "", func(tx pgx.Tx) error {
		err := payoutdb.New(tx).InsertQuote(ctx, payoutdb.InsertQuoteParams{
			ID:               uuid.New(),
			PayoutID:         p.ID,
			Rail:             s.rail.Name(),
			RailQuoteID:      rq.RailQuoteID,
			FiatAmount:       rq.FiatAmount,
			FiatCurrency:     rq.FiatCurrency,
			CryptoAuthorized: int64(rq.CryptoAuthorized),
			ExpiresAt:        rq.ExpiresAt,
		})
		if err != nil {
			return fmt.Errorf("payout: insert quote: %w", err)
		}
		return nil
	})
}

// Get returns a customer's payout. Another customer's payout is reported
// as ErrNotFound, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, customerID, id uuid.UUID) (Payout, error) {
	p, err := s.load(ctx, id)
	if err != nil {
		return Payout{}, err
	}
	if p.CustomerID != customerID {
		return Payout{}, ErrNotFound
	}
	return p, nil
}

// Confirm places the hold for a quoted payout (quoted -> held). It fails
// with ledger.ErrInsufficientFunds if the customer cannot cover the
// authorized amount (nothing changes), and with ErrQuoteExpired if the
// quote lapsed (the payout moves to expired). Confirming a payout that is
// already held or further along returns it unchanged.
func (s *Service) Confirm(ctx context.Context, customerID, id uuid.UUID) (Payout, error) {
	p, err := s.Get(ctx, customerID, id)
	if err != nil {
		return Payout{}, err
	}
	switch p.Status {
	case Quoted:
	case Held, Submitted, Settled, Reversed:
		return p, nil
	case Created, Failed, Expired:
		return p, fmt.Errorf("%w: payout is %s", ErrNotConfirmable, p.Status)
	}
	accts, err := ledger.FindCustomerAccounts(ctx, s.pool, p.CustomerID)
	if err != nil {
		return p, fmt.Errorf("payout: confirm: %w", err)
	}
	held, err := s.step(ctx, p, Held, "", func(tx pgx.Tx) error {
		// Checked inside the hold transaction, so a quote cannot lapse
		// between the check and the hold.
		if !s.now().Before(p.Quote.ExpiresAt) {
			return ErrQuoteExpired
		}
		if _, err := ledger.PostEntry(ctx, tx, ledger.Hold(p.ID, accts, p.Quote.CryptoAuthorized)); err != nil {
			return fmt.Errorf("payout: hold: %w", err)
		}
		return nil
	})
	if errors.Is(err, ErrQuoteExpired) {
		expired, err2 := s.step(ctx, p, Expired, "quote expired before confirm", nil)
		if err2 != nil {
			return p, err2
		}
		return expired, ErrQuoteExpired
	}
	return held, err
}

// Submit sends a held payout to the rail. The attempt's nonce is stored
// before the call, so a retry after a crash or timeout reuses it and the
// rail cannot create a second transaction.
//
//   - accepted: held -> submitted, then any status the rail already reports
//     is applied as by Sync;
//   - rejected: held -> failed and the hold is released;
//   - unknown outcome (timeout, 5xx): stays held with NeedsResolution set;
//     the payout is not released, because the rail may have accepted it.
//
// Submitting a payout that is not held returns it unchanged.
func (s *Service) Submit(ctx context.Context, id uuid.UUID) (Payout, error) {
	p, err := s.load(ctx, id)
	if err != nil {
		return Payout{}, err
	}
	if p.Status != Held {
		return p, nil
	}
	attempt, err := s.attempt(ctx, p)
	if err != nil {
		return p, err
	}

	rtx, err := s.rail.Submit(ctx, railQuote(p.Quote), attempt.Nonce, attempt.ExternalID)
	switch {
	case errors.Is(err, rail.ErrRejected):
		return s.step(ctx, p, Failed, "rail rejected: "+err.Error(), func(tx pgx.Tx) error {
			if err := s.setRailTxStatus(ctx, tx, attempt.ID, nil, rail.StatusFailed); err != nil {
				return err
			}
			return s.release(ctx, tx, p)
		})
	case err != nil:
		err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			n, err := payoutdb.New(tx).MarkNeedsResolution(ctx, payoutdb.MarkNeedsResolutionParams{
				ID: p.ID, Status: string(Held), Version: p.Version,
			})
			if err != nil {
				return fmt.Errorf("payout: mark needs resolution: %w", err)
			}
			if n == 0 {
				return errLost
			}
			return nil
		})
		if err != nil && !errors.Is(err, errLost) {
			return p, err
		}
		return s.load(ctx, p.ID)
	}

	if rtx.ExternalID != p.ID.String() {
		return p, fmt.Errorf("%w: rail tx %s belongs to %q", ErrRailMismatch, rtx.ID, rtx.ExternalID)
	}
	p, err = s.step(ctx, p, Submitted, "", func(tx pgx.Tx) error {
		return s.setRailTxStatus(ctx, tx, attempt.ID, &rtx.ID, rail.StatusPending)
	})
	if err != nil || p.Status != Submitted {
		return p, err
	}
	row, err := payoutdb.New(s.pool).LatestRailTx(ctx, p.ID)
	if err != nil {
		return p, fmt.Errorf("payout: reload rail tx: %w", err)
	}
	return s.apply(ctx, p, row, rtx)
}

// attempt returns the rail attempt to submit with: the open one if an
// earlier submit stored it, or a new one with a fresh nonce. A new attempt
// starts only after the previous one is confirmed failed.
func (s *Service) attempt(ctx context.Context, p Payout) (payoutdb.RailTransaction, error) {
	var row payoutdb.RailTransaction
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := payoutdb.New(tx)
		latest, err := q.LatestRailTx(ctx, p.ID)
		next := int32(1)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("payout: latest rail tx: %w", err)
		case latest.Status != string(rail.StatusFailed):
			row = latest
			return nil
		default:
			next = latest.Attempt + 1
		}
		row, err = q.InsertRailTx(ctx, payoutdb.InsertRailTxParams{
			PayoutID: p.ID, Attempt: next, Nonce: uuid.NewString(), ExternalID: p.ID.String(),
		})
		if errors.Is(err, pgx.ErrNoRows) { // a concurrent submit created it first
			row, err = q.LatestRailTx(ctx, p.ID)
		}
		if err != nil {
			return fmt.Errorf("payout: store rail attempt: %w", err)
		}
		return nil
	})
	return row, err
}

// Sync fetches the rail's current view of a submitted or settled payout
// and applies anything new: settlement, failure or a bank return. It is
// what a webhook triggers (M5): the webhook says "look again", and only
// the rail's own answer moves money.
func (s *Service) Sync(ctx context.Context, id uuid.UUID) (Payout, error) {
	p, err := s.load(ctx, id)
	if err != nil {
		return Payout{}, err
	}
	if p.Status != Submitted && p.Status != Settled {
		return p, nil
	}
	row, err := payoutdb.New(s.pool).LatestRailTx(ctx, p.ID)
	if err != nil {
		return p, fmt.Errorf("payout: rail tx: %w", err)
	}
	if row.RailTxID == nil {
		return p, nil
	}
	rtx, err := s.rail.Get(ctx, *row.RailTxID)
	if err != nil {
		return p, fmt.Errorf("payout: fetch rail tx: %w", err)
	}
	return s.apply(ctx, p, row, rtx)
}

// apply moves the payout to match the rail's report. Reports no newer than
// the last one applied are ignored, so duplicate and out-of-order
// deliveries change nothing.
func (s *Service) apply(ctx context.Context, p Payout, row payoutdb.RailTransaction, rtx rail.Tx) (Payout, error) {
	if rtx.ExternalID != p.ID.String() {
		return p, fmt.Errorf("%w: rail tx %s belongs to %q", ErrRailMismatch, rtx.ID, rtx.ExternalID)
	}
	switch {
	case rtx.Version < row.LastEventVersion:
		return p, nil
	case rtx.Version == row.LastEventVersion:
		// Same progress, maybe a new sub-status (a review moving on).
		// Recorded for visibility; it never moves money.
		return p, s.recordSubStatus(ctx, row, rtx)
	}
	record := func(tx pgx.Tx) error {
		sub := &rtx.SubStatus
		if rtx.SubStatus == "" {
			sub = nil
		}
		n, err := payoutdb.New(tx).UpdateRailTx(ctx, payoutdb.UpdateRailTxParams{
			ID: row.ID, RailTxID: &rtx.ID, Status: string(rtx.Status), SubStatus: sub, EventVersion: rtx.Version,
		})
		if err != nil {
			return fmt.Errorf("payout: record rail event: %w", err)
		}
		if n == 0 {
			return errLost
		}
		return nil
	}
	accts, err := ledger.FindCustomerAccounts(ctx, s.pool, p.CustomerID)
	if err != nil {
		return p, fmt.Errorf("payout: apply: %w", err)
	}

	switch {
	case p.Status == Submitted && rtx.Status == rail.StatusSettled:
		e, err := ledger.Settle(p.ID, s.house, accts, p.Quote.CryptoAuthorized, rtx.Spent, rtx.Fee)
		if err != nil {
			return p, fmt.Errorf("%w: %w", ErrRailMismatch, err)
		}
		p, err = s.step(ctx, p, Settled, "", func(tx pgx.Tx) error {
			if err := record(tx); err != nil {
				return err
			}
			if err := post(ctx, tx, e); err != nil {
				return err
			}
			if p.DestinationID == uuid.Nil || rtx.PaymentMethodID == "" {
				return nil
			}
			return s.dests.RememberPaymentMethod(ctx, tx, p.DestinationID, rtx.PaymentMethodID)
		})
		if err != nil || p.Status != Settled || rtx.Return == nil {
			return p, err
		}
		return s.reverse(ctx, p, accts, rtx, nil)

	case p.Status == Submitted && rtx.Status == rail.StatusFailed:
		return s.step(ctx, p, Failed, "rail failed the payout", func(tx pgx.Tx) error {
			if err := record(tx); err != nil {
				return err
			}
			return s.release(ctx, tx, p)
		})

	case p.Status == Settled && rtx.Return != nil:
		return s.reverse(ctx, p, accts, rtx, record)

	default: // e.g. a new sub-status while pending: record it, no state change
		err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, record)
		if err != nil && !errors.Is(err, errLost) {
			return p, err
		}
		return s.load(ctx, p.ID)
	}
}

// recordSubStatus stores a sub-status reported at the version already
// applied. A different status at the same version would contradict the
// rail's own ordering and is ignored.
func (s *Service) recordSubStatus(ctx context.Context, row payoutdb.RailTransaction, rtx rail.Tx) error {
	if string(rtx.Status) != row.Status {
		return nil
	}
	var sub *string
	if rtx.SubStatus != "" {
		sub = &rtx.SubStatus
	}
	_, err := payoutdb.New(s.pool).UpdateRailSubStatus(ctx, payoutdb.UpdateRailSubStatusParams{
		ID: row.ID, SubStatus: sub, EventVersion: rtx.Version,
	})
	if err != nil {
		return fmt.Errorf("payout: record sub-status: %w", err)
	}
	return nil
}

// reverse books a bank return (settled -> reversed), crediting only the
// amount the bank sent back.
func (s *Service) reverse(ctx context.Context, p Payout, accts ledger.CustomerAccounts, rtx rail.Tx, record func(pgx.Tx) error) (Payout, error) {
	ret := rtx.Return
	if ret.Amount <= 0 || ret.Amount > rtx.Spent {
		return p, fmt.Errorf("%w: return of %s for a payout that spent %s", ErrRailMismatch, ret.Amount, rtx.Spent)
	}
	return s.step(ctx, p, Reversed, "returned by the bank", func(tx pgx.Tx) error {
		if record != nil {
			if err := record(tx); err != nil {
				return err
			}
		}
		return post(ctx, tx, ledger.BankReturn(p.ID, ret.RailTxID, s.house, accts, ret.Amount))
	})
}

func (s *Service) release(ctx context.Context, tx pgx.Tx, p Payout) error {
	accts, err := ledger.FindCustomerAccounts(ctx, tx, p.CustomerID)
	if err != nil {
		return fmt.Errorf("payout: release: %w", err)
	}
	return post(ctx, tx, ledger.Release(p.ID, accts, p.Quote.CryptoAuthorized))
}

func (s *Service) setRailTxStatus(ctx context.Context, tx pgx.Tx, id int64, railTxID *string, st rail.Status) error {
	err := payoutdb.New(tx).SetRailTxStatus(ctx, payoutdb.SetRailTxStatusParams{ID: id, RailTxID: railTxID, Status: string(st)})
	if err != nil {
		return fmt.Errorf("payout: set rail tx status: %w", err)
	}
	return nil
}

func post(ctx context.Context, tx pgx.Tx, e ledger.Entry) error {
	if _, err := ledger.PostEntry(ctx, tx, e); err != nil {
		return fmt.Errorf("payout: post %s: %w", e.Type, err)
	}
	return nil
}

// event is the outbox payload for every payout transition.
type event struct {
	PayoutID uuid.UUID `json:"payout_id"`
	From     State     `json:"from,omitempty"`
	To       State     `json:"to"`
}

// step performs one transition in one transaction: the guarded status
// update, then extra (ledger entry, rail row), then the outbox event. If
// another request moved the payout first, nothing is written and the
// current payout is returned without an error.
func (s *Service) step(ctx context.Context, p Payout, to State, reason string, extra func(pgx.Tx) error) (Payout, error) {
	if !CanTransition(p.Status, to) {
		return p, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, p.Status, to)
	}
	var why *string
	if reason != "" {
		why = &reason
	}
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		n, err := payoutdb.New(tx).Transition(ctx, payoutdb.TransitionParams{
			ID: p.ID, FromStatus: string(p.Status), ToStatus: string(to), Version: p.Version, FailureReason: why,
		})
		if err != nil {
			return fmt.Errorf("payout: transition: %w", err)
		}
		if n == 0 {
			return errLost
		}
		if extra != nil {
			if err := extra(tx); err != nil {
				return err
			}
		}
		return outbox.Write(ctx, tx, "payout."+string(to), p.ID, event{PayoutID: p.ID, From: p.Status, To: to})
	})
	if err != nil && !errors.Is(err, errLost) {
		return p, err
	}
	return s.load(ctx, p.ID)
}

func (s *Service) load(ctx context.Context, id uuid.UUID) (Payout, error) {
	q := payoutdb.New(s.pool)
	r, err := q.GetPayout(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Payout{}, ErrNotFound
	}
	if err != nil {
		return Payout{}, fmt.Errorf("payout: load: %w", err)
	}
	p := Payout{
		ID: r.ID, CustomerID: r.CustomerID, IdempotencyKey: r.IdempotencyKey,
		FiatAmount: r.FiatAmount, FiatCurrency: r.FiatCurrency, Destination: r.Destination,
		Status: State(r.Status), NeedsResolution: r.NeedsResolution, Version: r.Version,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.FailureReason != nil {
		p.FailureReason = *r.FailureReason
	}
	if r.DestinationID != nil {
		p.DestinationID = *r.DestinationID
	}
	qr, err := q.GetQuote(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Payout{}, fmt.Errorf("payout: load quote: %w", err)
	default:
		p.Quote = &Quote{
			ID: qr.ID, Rail: qr.Rail, RailQuoteID: qr.RailQuoteID,
			FiatAmount: qr.FiatAmount, FiatCurrency: qr.FiatCurrency,
			CryptoAuthorized: ledger.Amount(qr.CryptoAuthorized), ExpiresAt: qr.ExpiresAt,
		}
	}
	return p, nil
}

func railQuote(q *Quote) rail.Quote {
	return rail.Quote{
		RailQuoteID:      q.RailQuoteID,
		FiatAmount:       q.FiatAmount,
		FiatCurrency:     q.FiatCurrency,
		CryptoAuthorized: q.CryptoAuthorized,
		ExpiresAt:        q.ExpiresAt,
	}
}
