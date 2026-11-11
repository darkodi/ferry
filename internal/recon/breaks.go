package recon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/recon/recondb"
)

// StoredBreak is a break as stored, for the operator API.
type StoredBreak struct {
	ID             uuid.UUID
	Check          string
	Kind           string
	Subject        string
	Detail         map[string]string
	FirstSeen      time.Time
	LastSeen       time.Time
	SeenCount      int32
	ResolvedAt     *time.Time
	ResolvedReason string
}

// Errors from Resolve.
var (
	ErrBreakNotOpen = errors.New("recon: no open break with that id")
	ErrBadReason    = errors.New("recon: a resolution needs a reason of 3-2000 printable characters")
)

// ListBreaks returns live breaks (not cleared), unresolved first and
// oldest first; resolved ones too if includeResolved.
func ListBreaks(ctx context.Context, db recondb.DBTX, includeResolved bool, limit int32) ([]StoredBreak, error) {
	rows, err := recondb.New(db).ListBreaks(ctx, recondb.ListBreaksParams{IncludeResolved: includeResolved, MaxRows: limit})
	if err != nil {
		return nil, fmt.Errorf("recon: list breaks: %w", err)
	}
	out := make([]StoredBreak, 0, len(rows))
	for _, r := range rows {
		b := StoredBreak{
			ID: r.ID, Check: r.CheckName, Kind: r.Kind, Subject: r.Subject, FirstSeen: r.FirstSeen,
			LastSeen: r.LastSeen, SeenCount: r.SeenCount, ResolvedAt: r.ResolvedAt,
		}
		if r.ResolvedReason != nil {
			b.ResolvedReason = *r.ResolvedReason
		}
		if err := json.Unmarshal(r.Detail, &b.Detail); err != nil {
			return nil, fmt.Errorf("recon: break %s detail: %w", r.ID, err)
		}
		out = append(out, b)
	}
	return out, nil
}

// Resolve marks an open break as explained by a person. It changes no
// money: if the explanation is "the ledger is wrong", the fix is a
// correction entry posted separately, naming this break's id.
func Resolve(ctx context.Context, db recondb.DBTX, id uuid.UUID, reason string) error {
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 || len(reason) > 2000 || strings.ContainsFunc(reason, func(r rune) bool { return !unicode.IsPrint(r) && r != '\n' }) {
		return ErrBadReason
	}
	n, err := recondb.New(db).ResolveBreak(ctx, recondb.ResolveBreakParams{ID: id, Reason: &reason})
	if err != nil {
		return fmt.Errorf("recon: resolve break: %w", err)
	}
	if n == 0 {
		return ErrBreakNotOpen
	}
	return nil
}

// OpenCount is the number of open (unresolved, uncleared) breaks.
func OpenCount(ctx context.Context, db recondb.DBTX) (int64, error) {
	n, err := recondb.New(db).OpenBreakCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("recon: count open breaks: %w", err)
	}
	return n, nil
}
