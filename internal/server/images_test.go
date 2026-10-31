package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getitdone-gmbh/agent-fabrik/internal/app"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/server"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tenant"
)

// A 1x1 PNG.
const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// visionLLM answers with the number of images in the last user message.
func visionLLM(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("llm decode: %v", err)
		}
		n := 0
		var parts []struct{ Type string }
		if json.Unmarshal(req.Messages[len(req.Messages)-1].Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "image_url" {
					n++
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("IMAGES: %d", n)}, "finish_reason": "stop"}},
		})
	}))
}

func visionSetup(t *testing.T) *env {
	llmSrv := visionLLM(t)
	t.Cleanup(llmSrv.Close)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server:
  auth_token: secret-token
providers:
  fake:
    type: openai-compatible
    base_url: %s
agents:
  eye:
    description: looks at pictures
`, llmSrv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Build(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	m, err := tenant.New(ctx, cfg, nil, a, log)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	m.Start(ctx, &wg)
	srv := httptest.NewServer(server.New(ctx, m, cfg))
	t.Cleanup(func() { srv.Close(); cancel(); wg.Wait(); a.Close() })
	return &env{srv: srv}
}

func TestRunWithImages(t *testing.T) {
	e := visionSetup(t)
	body := fmt.Sprintf(`{"input":"was ist das?","images":["data:image/png;base64,%s",{"data":%q,"media_type":"image/png"}]}`, pngB64, pngB64)
	resp, out := e.do(t, "POST", "/v1/agents/eye/run", body, auth)
	if resp.StatusCode != 200 || !strings.Contains(out, "IMAGES: 2") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	// An image alone is a valid input.
	resp, out = e.do(t, "POST", "/v1/agents/eye/run", fmt.Sprintf(`{"images":[%q]}`, pngB64), auth)
	if resp.StatusCode != 200 || !strings.Contains(out, "IMAGES: 1") {
		t.Fatalf("image only: %d %s", resp.StatusCode, out)
	}
}

func TestRunRejectsBadImages(t *testing.T) {
	e := visionSetup(t)
	for name, imgs := range map[string]string{
		"link":     `["https://example.com/x.png"]`,
		"not png":  fmt.Sprintf("[%q]", base64.StdEncoding.EncodeToString([]byte("hello world"))),
		"too many": fmt.Sprintf("[%q,%q,%q,%q,%q]", pngB64, pngB64, pngB64, pngB64, pngB64),
	} {
		resp, out := e.do(t, "POST", "/v1/agents/eye/run", `{"input":"x","images":`+imgs+`}`, auth)
		if resp.StatusCode != 400 {
			t.Errorf("%s: want 400, got %d %s", name, resp.StatusCode, out)
		}
	}
}

func TestRunBodyLimitAllowsLargeImages(t *testing.T) {
	e := visionSetup(t)
	// ~3 MB: above the 1 MiB API body limit, below the run limit. The
	// padding is ignored by the PNG sniffer but counts against the size.
	raw, _ := base64.StdEncoding.DecodeString(pngB64)
	big := base64.StdEncoding.EncodeToString(append(raw, make([]byte, 3<<20)...))
	resp, out := e.do(t, "POST", "/v1/agents/eye/run", fmt.Sprintf(`{"input":"x","images":[%q]}`, big), auth)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	// Other routes keep the small limit.
	resp, _ = e.do(t, "PUT", "/v1/config", `{"x":"`+strings.Repeat("a", 2<<20)+`"}`, auth)
	if resp.StatusCode < 400 {
		t.Errorf("config route accepted a 2 MB body: %d", resp.StatusCode)
	}
}

func TestChatCompletionsWithImage(t *testing.T) {
	e := visionSetup(t)
	body := fmt.Sprintf(`{"model":"eye","messages":[{"role":"user","content":[{"type":"text","text":"was ist das?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, pngB64)
	resp, out := e.do(t, "POST", "/v1/chat/completions", body, auth)
	if resp.StatusCode != 200 || !strings.Contains(out, "IMAGES: 1") {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	body = `{"model":"eye","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://169.254.169.254/x.png"}}]}]}`
	if resp, out := e.do(t, "POST", "/v1/chat/completions", body, auth); resp.StatusCode != 400 {
		t.Errorf("remote image URL must be refused, got %d %s", resp.StatusCode, out)
	}
}
