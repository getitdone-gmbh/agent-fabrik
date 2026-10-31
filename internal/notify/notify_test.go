package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebhook(t *testing.T) {
	var calls atomic.Int32
	var status atomic.Int32
	status.Store(500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var e Event
		if r.Header.Get("Authorization") != "Bearer s3cret" || json.NewDecoder(r.Body).Decode(&e) != nil || e.Agent != "support" {
			w.WriteHeader(401)
			return
		}
		code := int(status.Load())
		status.Store(200) // fail once, then accept
		w.WriteHeader(code)
	}))
	defer srv.Close()
	w := &Webhook{URL: srv.URL, Token: "s3cret"}
	ctx := context.Background()
	if err := w.Notify(ctx, Event{Type: TypeAlert, Tenant: "t", Agent: "support", At: time.Now()}); err != nil || calls.Load() != 2 {
		t.Fatalf("retry after 500: %v, %d calls", err, calls.Load())
	}
	calls.Store(0)
	bad := &Webhook{URL: srv.URL, Token: "wrong"}
	if err := bad.Notify(ctx, Event{Agent: "support"}); err == nil || calls.Load() != 1 {
		t.Errorf("4xx must fail without retry: %v, %d calls", err, calls.Load())
	}
}
