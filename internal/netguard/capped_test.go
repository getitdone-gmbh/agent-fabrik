package netguard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCappedFailsInsteadOfTruncating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()
	get := func(max int64) (string, error) {
		c := &http.Client{Transport: capped{http.DefaultTransport, max}}
		resp, err := c.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	if b, err := get(100); err != nil || len(b) != 100 {
		t.Errorf("exactly at the cap: %d bytes, %v", len(b), err)
	}
	if _, err := get(99); err == nil || !strings.Contains(err.Error(), "larger than 99") {
		t.Errorf("over the cap must fail, got %v", err)
	}
}
