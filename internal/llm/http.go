package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// APIError is a non-2xx response from a provider.
type APIError struct {
	Provider string
	Status   int
	Body     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Status, e.Body)
}

// Retryable reports whether the request may succeed on retry.
func (e *APIError) Retryable() bool {
	return e.Status == 408 || e.Status == 409 || e.Status == 429 || e.Status >= 500
}

type httpClient struct {
	name       string
	client     *http.Client
	maxRetries int
}

func newHTTPClient(name string, c Config) httpClient {
	timeout := 10 * time.Minute
	if c.TimeoutSec > 0 {
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}
	retries := 2
	if c.MaxRetries != nil {
		retries = *c.MaxRetries
	}
	return httpClient{name: name, client: &http.Client{Timeout: timeout, Transport: c.Transport}, maxRetries: retries}
}

// postJSON sends body to url and decodes the JSON response into out,
// retrying transient failures with exponential backoff.
func (h httpClient) postJSON(ctx context.Context, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= h.maxRetries; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * time.Second
			if ae, ok := lastErr.(*APIError); ok && ae.Status == 429 {
				wait *= 2
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		lastErr = h.do(ctx, url, headers, payload, out)
		if lastErr == nil {
			return nil
		}
		if ae, ok := lastErr.(*APIError); ok && !ae.Retryable() {
			return lastErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

func (h httpClient) do(ctx context.Context, url string, headers map[string]string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		body := string(data)
		if len(body) > 2000 {
			body = body[:2000] + "…"
		}
		return &APIError{Provider: h.name, Status: resp.StatusCode, Body: body}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", h.name, err)
	}
	return nil
}

var idCounter atomic.Uint64

// newCallID generates a local tool call ID for providers that don't return one.
func newCallID() string {
	return "call_" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(idCounter.Add(1), 36)
}
