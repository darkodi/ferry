package api_test

import (
	"net/http"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// One trace follows a payout across processes: the HTTP confirm writes the
// payout.held event with its traceparent, the relay publishes it, and the
// worker's span (and the submit under it) continue the same trace.
func TestTraceCrossesTheQueue(t *testing.T) {
	_, key := newCustomer(t, "200")
	r := call(t, "POST", "/v1/payouts", key, "trace-1", payoutBody(newDestination(t, key), 100_00))
	wantStatus(t, r, http.StatusCreated)
	pid := r.json(t)["id"].(string)
	wantStatus(t, call(t, "POST", "/v1/payouts/"+pid+"/confirm", key, "trace-2", nil), http.StatusOK)

	var confirm trace.TraceID
	for _, s := range spans.Ended() {
		if s.Name() == "HTTP POST /v1/payouts/{id}/confirm" {
			confirm = s.SpanContext().TraceID()
		}
	}
	if !confirm.IsValid() {
		t.Fatal("no span for the confirm request")
	}

	for {
		published, err := relay.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		received, err := worker.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if published == 0 && received == 0 {
			break
		}
	}
	want := map[string]bool{"outbox.publish payout.held": false, "worker payout.held": false, "payout.submit": false}
	for _, s := range spans.Ended() {
		if _, ok := want[s.Name()]; ok && s.SpanContext().TraceID() == confirm {
			want[s.Name()] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("no %q span in the confirm request's trace %s (spans: %v)", name, confirm, names(spans.Ended()))
		}
	}
}

func names(ss []sdktrace.ReadOnlySpan) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name()
	}
	return out
}
