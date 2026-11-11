package noah

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/darkodi/ferry/internal/platform/obs"
)

// maxResponse bounds how much of a Noah response is read.
const maxResponse = 1 << 20

// APIError is Noah's error envelope, returned for every non-2xx answer.
// See https://docs.noah.com/api-concepts/errors.
type APIError struct {
	Status   int    `json:"-"`
	Type     string `json:"Type"` // InvalidMessage, Unauthorized, Forbidden, ResourceNotFound, InsufficientBalance, Conflict, Unexpected
	Action   string `json:"Action"`
	Detail   string `json:"Detail"`
	Instance string `json:"Instance"`
}

func (e *APIError) Error() string {
	t := e.Type
	if t == "" {
		t = http.StatusText(e.Status)
	}
	if e.Detail != "" {
		return fmt.Sprintf("noah: %d %s: %s (instance %s)", e.Status, t, e.Detail, e.Instance)
	}
	return fmt.Sprintf("noah: %d %s (instance %s)", e.Status, t, e.Instance)
}

// Client calls the Noah Business API. Every request carries X-Api-Key and,
// when a signer is set, an Api-Signature JWT over its exact bytes.
//
// It never logs: request bodies hold bank details and headers hold the
// API key.
type Client struct {
	base   *url.URL // e.g. https://api.sandbox.noah.com/v1
	apiKey string
	signer *Signer // nil sends unsigned requests (sandbox keys without a signing key)
	http   *http.Client
}

// NewClient returns a client for baseURL. signer may be nil only for the
// sandbox: the production API rejects unsigned requests, so a production
// base URL without a signer is refused here, at startup.
func NewClient(baseURL, apiKey string, signer *Signer) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("noah: invalid base URL %q", baseURL)
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("noah: base URL must use https (got %s)", u.Scheme)
	}
	if apiKey == "" {
		return nil, errors.New("noah: API key is required")
	}
	if signer == nil && u.Hostname() == "api.noah.com" {
		return nil, errors.New("noah: production requires request signing; set NOAH_SIGNING_KEY_PATH")
	}
	return &Client{
		base:   u,
		apiKey: apiKey,
		signer: signer,
		http: &http.Client{
			Timeout: 10 * time.Second,
			// Never follow redirects: a redirect would resend the API key
			// and signed body to wherever Noah (or a proxy) pointed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// do sends one request and decodes a 2xx JSON answer into out. Any other
// status comes back as *APIError. Unknown response fields are ignored:
// Noah adds fields over time, and Ferry checks the ones it uses.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) (err error) {
	// One span per call: method, path and status only. Bodies (bank
	// details) and headers (the API key) never go into traces.
	ctx, span := obs.Tracer.Start(ctx, "noah "+method+" "+path, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("http.method", method), attribute.String("url.path", path)))
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, "noah call failed")
			if st := statusOf(err); st != 0 {
				span.SetAttributes(attribute.Int("http.status_code", st))
			}
		}
		span.End()
	}()
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("noah: encode %s %s: %w", method, path, err)
		}
	}
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("noah: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.signer != nil {
		// The claim's path is the URL path as sent, /v1 prefix included;
		// the hash covers exactly the bytes in the request body.
		sig, err := c.signer.Sign(ctx, method, u.Path, query, body)
		if err != nil {
			return err
		}
		req.Header.Set("Api-Signature", sig)
	}

	resp, err := c.http.Do(req)
	if err == nil {
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
	}
	if err != nil {
		return fmt.Errorf("noah: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("noah: read %s %s: %w", method, path, err)
	}
	if len(raw) > maxResponse {
		return fmt.Errorf("noah: %s %s: response larger than %d bytes", method, path, maxResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, apiErr) // a non-JSON error body still yields the status
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("noah: decode %s %s: %w", method, path, err)
	}
	return nil
}

// statusOf returns the HTTP status of a Noah error, or 0 if err did not
// come from an answer (network failure, timeout).
func statusOf(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}
