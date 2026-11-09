package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/darkodi/ferry/internal/rail"
)

// maxWebhookBody bounds a rail webhook. Noah's transaction events are a
// few kilobytes.
const maxWebhookBody = 1 << 20

// railWebhook receives the payout rail's webhooks.
//
// The order is what makes it safe: verify the signature over the raw body
// (401 and nothing stored if it fails), then store the event and queue a
// re-fetch in one transaction, and only then answer 2xx. A failure before
// the commit answers 5xx, and the rail retries. The event itself never
// moves money: the worker asks the rail for the transaction's current
// state and applies that.
func (s *server) railWebhook(w http.ResponseWriter, r *http.Request) {
	hooks := s.RailWebhooks
	if chi.URLParam(r, "rail") != hooks.Name() {
		writeError(w, r, http.StatusNotFound, "not_found", "not found")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, r, http.StatusRequestEntityTooLarge, "too_large", "webhook body too large")
		return
	}
	ev, err := hooks.Parse(body, r.Header)
	switch {
	case errors.Is(err, rail.ErrBadSignature):
		s.Log.WarnContext(r.Context(), "rail webhook with a bad signature", "rail", hooks.Name(), "remote", r.RemoteAddr)
		writeError(w, r, http.StatusUnauthorized, "bad_signature", "signature does not verify")
		return
	case errors.Is(err, rail.ErrIgnoredEvent):
		writeJSON(w, http.StatusOK, map[string]any{"received": true})
		return
	case err != nil:
		// Signed but not understood: refuse, and keep it visible in the
		// rail's retries and our logs.
		s.Log.WarnContext(r.Context(), "rail webhook not understood", "rail", hooks.Name(), "error", err)
		writeError(w, r, http.StatusBadRequest, "invalid_event", err.Error())
		return
	}
	res, err := s.Payouts.RecordRailEvent(r.Context(), ev)
	if err != nil {
		s.internal(w, r, "record rail event", err)
		return
	}
	s.Log.InfoContext(r.Context(), "rail webhook",
		"rail", ev.Rail, "rail_tx_id", ev.RailTxID, "event_version", ev.Version,
		"payout_id", res.PayoutID, "stored", res.Stored, "queued", res.Queued)
	writeJSON(w, http.StatusOK, map[string]any{"received": true})
}
