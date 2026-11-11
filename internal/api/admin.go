package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/darkodi/ferry/internal/recon"
	"github.com/darkodi/ferry/internal/treasury"
)

// ---- treasury ----

type topupReq struct {
	RailTxID string `json:"rail_tx_id"`
}

// recordTopup books a treasury top-up from the rail transaction the
// operator names. Ferry fetches it from the rail and books the rail's
// amount; the operator never supplies one. Repeating it is harmless.
func (s *server) recordTopup(w http.ResponseWriter, r *http.Request) {
	var req topupReq
	if !decode(w, r, &req) {
		return
	}
	t, err := s.Treasury.Record(r.Context(), req.RailTxID)
	switch {
	case errors.Is(err, treasury.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, treasury.ErrNotTopup):
		writeError(w, r, http.StatusUnprocessableEntity, "not_a_topup", err.Error())
	case err != nil:
		s.internal(w, r, "record top-up", err)
	default:
		status := http.StatusCreated
		if !t.New {
			status = http.StatusOK
		}
		s.Log.InfoContext(r.Context(), "treasury top-up", "rail_tx_id", t.RailTxID, "amount", t.Amount.String(), "new", t.New)
		writeJSON(w, status, map[string]any{"rail_tx_id": t.RailTxID, "amount": t.Amount.String(), "new": t.New})
	}
}

// ---- reconciliation breaks ----

type breakResp struct {
	ID             uuid.UUID         `json:"id"`
	Check          string            `json:"check"`
	Kind           string            `json:"kind"`
	Subject        string            `json:"subject"`
	Detail         map[string]string `json:"detail"`
	FirstSeen      time.Time         `json:"first_seen"`
	LastSeen       time.Time         `json:"last_seen"`
	SeenCount      int32             `json:"seen_count"`
	ResolvedAt     *time.Time        `json:"resolved_at,omitempty"`
	ResolvedReason string            `json:"resolved_reason,omitempty"`
}

// listBreaks lists live reconciliation breaks; ?resolved=true includes
// resolved ones.
func (s *server) listBreaks(w http.ResponseWriter, r *http.Request) {
	bs, err := recon.ListBreaks(r.Context(), s.Pool, r.URL.Query().Get("resolved") == "true", 500)
	if err != nil {
		s.internal(w, r, "list breaks", err)
		return
	}
	out := make([]breakResp, len(bs))
	for i, b := range bs {
		out[i] = breakResp{
			ID: b.ID, Check: b.Check, Kind: b.Kind, Subject: b.Subject, Detail: b.Detail,
			FirstSeen: b.FirstSeen, LastSeen: b.LastSeen, SeenCount: b.SeenCount,
			ResolvedAt: b.ResolvedAt, ResolvedReason: b.ResolvedReason,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"breaks": out})
}

type resolveReq struct {
	Reason string `json:"reason"`
}

// resolveBreak records a person's explanation of a break. It moves no
// money; a ledger fix is a separate correction entry naming the break.
func (s *server) resolveBreak(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req resolveReq
	if !decode(w, r, &req) {
		return
	}
	err := recon.Resolve(r.Context(), s.Pool, id, req.Reason)
	switch {
	case errors.Is(err, recon.ErrBadReason):
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error())
	case errors.Is(err, recon.ErrBreakNotOpen):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		s.internal(w, r, "resolve break", err)
	default:
		s.Log.InfoContext(r.Context(), "recon break resolved", "break_id", id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "resolved": true})
	}
}
