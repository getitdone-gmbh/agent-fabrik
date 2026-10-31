package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A 1x1 PNG.
var pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestParseImage(t *testing.T) {
	for name, in := range map[string]string{"base64": pngB64, "data URL": "data:image/png;base64," + pngB64} {
		img, err := ParseImage(in, "")
		if err != nil || img.MediaType != "image/png" || img.Data != pngB64 {
			t.Errorf("%s: %+v %v", name, img, err)
		}
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, MaxImageBytes+1))
	for name, c := range map[string]struct{ in, mt string }{
		"link":       {"https://example.com/a.png", ""},
		"not base64": {"!!!", ""},
		"not image":  {base64.StdEncoding.EncodeToString([]byte("<html>hi</html>")), ""},
		"wrong type": {pngB64, "image/jpeg"},
		"too big":    {big, ""},
		"data utf8":  {"data:image/png," + pngB64, ""},
	} {
		if _, err := ParseImage(c.in, c.mt); !errors.Is(err, ErrImage) {
			t.Errorf("%s: want ErrImage, got %v", name, err)
		}
	}
}

func TestWithoutImages(t *testing.T) {
	img := Image{MediaType: "image/png", Data: pngB64}
	h := []Message{{Role: RoleUser, Content: "was ist das?", Images: []Image{img}}, {Role: RoleAssistant, Content: "Ein Pixel."}}
	out := WithoutImages(h)
	if out[0].Images != nil || !strings.Contains(out[0].Content, "was ist das?") || !strings.Contains(out[0].Content, "1 image(s)") {
		t.Errorf("stripped: %+v", out[0])
	}
	if h[0].Images == nil {
		t.Error("input history was modified")
	}
	plain := []Message{{Role: RoleUser, Content: "x"}}
	if got := WithoutImages(plain); &got[0] != &plain[0] {
		t.Error("history without images should be returned as is")
	}
}

func capture(t *testing.T, path string, reply string) (*httptest.Server, *map[string]any) {
	got := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func imageRequest() Request {
	return Request{Messages: []Message{{Role: RoleUser, Content: "was ist das?", Images: []Image{{MediaType: "image/png", Data: pngB64}}}}}
}

const oaReply = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

func userParts(t *testing.T, got map[string]any) []any {
	msgs := got["messages"].([]any)
	parts, ok := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("user content is not a part list: %v", msgs)
	}
	return parts
}

func TestOpenAIImages(t *testing.T) {
	srv, got := capture(t, "/chat/completions", oaReply)
	p, _ := New("qwen", Config{Type: "vllm", BaseURL: srv.URL, Model: "m"})
	if _, err := p.Chat(context.Background(), imageRequest()); err != nil {
		t.Fatal(err)
	}
	parts := userParts(t, *got)
	if len(parts) != 2 || parts[0].(map[string]any)["text"] != "was ist das?" {
		t.Fatalf("parts: %v", parts)
	}
	u := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if u != "data:image/png;base64,"+pngB64 {
		t.Errorf("image_url: %v", u)
	}
}

func TestMistralImagesAsString(t *testing.T) {
	srv, got := capture(t, "/chat/completions", oaReply)
	p, _ := New("m", Config{Type: "mistral", BaseURL: srv.URL})
	if _, err := p.Chat(context.Background(), imageRequest()); err != nil {
		t.Fatal(err)
	}
	if u := userParts(t, *got)[1].(map[string]any)["image_url"]; u != "data:image/png;base64,"+pngB64 {
		t.Errorf("mistral image_url should be a plain data URL, got %v", u)
	}
}

func TestOpenAIWithoutImagesStaysString(t *testing.T) {
	srv, got := capture(t, "/chat/completions", oaReply)
	p, _ := New("m", Config{Type: "vllm", BaseURL: srv.URL})
	if _, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if c := (*got)["messages"].([]any)[0].(map[string]any)["content"]; c != "hi" {
		t.Errorf("content: %v", c)
	}
}

func TestAnthropicImages(t *testing.T) {
	srv, got := capture(t, "/v1/messages", `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	p, _ := New("c", Config{Type: "anthropic", BaseURL: srv.URL, APIKey: "k"})
	if _, err := p.Chat(context.Background(), imageRequest()); err != nil {
		t.Fatal(err)
	}
	blocks := (*got)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	first := blocks[0].(map[string]any)
	src := first["source"].(map[string]any)
	if first["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != pngB64 {
		t.Errorf("image block: %v", first)
	}
	if blocks[1].(map[string]any)["text"] != "was ist das?" {
		t.Errorf("text block: %v", blocks[1])
	}
}

func TestOllamaImages(t *testing.T) {
	srv, got := capture(t, "/api/chat", `{"message":{"role":"assistant","content":"ok"},"done":true}`)
	p, _ := New("o", Config{Type: "ollama", BaseURL: srv.URL, Model: "m"})
	if _, err := p.Chat(context.Background(), imageRequest()); err != nil {
		t.Fatal(err)
	}
	m := (*got)["messages"].([]any)[0].(map[string]any)
	if imgs := m["images"].([]any); len(imgs) != 1 || imgs[0] != pngB64 {
		t.Errorf("ollama images: %v", m)
	}
}
