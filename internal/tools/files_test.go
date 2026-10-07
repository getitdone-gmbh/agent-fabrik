package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type memFiles map[string][]byte

func (m memFiles) List(_ context.Context, prefix string) ([]FileInfo, error) {
	var out []FileInfo
	for p, d := range m {
		if strings.HasPrefix(p, prefix) {
			out = append(out, FileInfo{Path: p, Size: int64(len(d))})
		}
	}
	return out, nil
}
func (m memFiles) Read(_ context.Context, p string) (FileInfo, []byte, error) {
	d, ok := m[p]
	if !ok {
		return FileInfo{}, nil, errors.New("file not found: " + p)
	}
	return FileInfo{Path: p, Size: int64(len(d)), ContentType: "application/pdf"}, d, nil
}
func (m memFiles) Write(_ context.Context, p string, d []byte) (FileInfo, error) {
	m[p] = d
	return FileInfo{Path: p, Size: int64(len(d))}, nil
}
func (m memFiles) Delete(_ context.Context, p string) error { delete(m, p); return nil }

func TestFilesTools(t *testing.T) {
	store := memFiles{"scan.pdf": {0xff, 0xfe, 0x00}}
	ts, _ := NewFiles("files", store).Tools(context.Background())
	byName := map[string]Tool{}
	for _, tl := range ts {
		byName[tl.Local] = tl
	}
	call := func(name string, args any) (string, error) {
		b, _ := json.Marshal(args)
		return byName[name].Handler(context.Background(), b)
	}
	if _, err := call("write", map[string]any{"path": "log.md", "content": "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call("write", map[string]any{"path": "log.md", "content": "b", "append": true}); err != nil {
		t.Fatal(err)
	}
	if string(store["log.md"]) != "ab" {
		t.Fatalf("append: %q", store["log.md"])
	}
	parse := func(s string) map[string]any {
		var m map[string]any
		_ = json.Unmarshal([]byte(s), &m)
		return m
	}
	out, err := call("read", map[string]any{"path": "log.md"})
	if err != nil || parse(out)["content"] != "ab" {
		t.Fatalf("read: %s %v", out, err)
	}
	if _, err := call("read", map[string]any{"path": "scan.pdf"}); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary read: %v", err)
	}
	store["long.txt"] = []byte(strings.Repeat("ä", maxReadChars+10))
	out, _ = call("read", map[string]any{"path": "long.txt"})
	if parse(out)["next_offset"] != float64(maxReadChars) {
		t.Fatalf("paging: %.200s", out)
	}
	out, _ = call("read", map[string]any{"path": "long.txt", "offset": maxReadChars})
	if parse(out)["content"] != "ääääääääää" {
		t.Fatalf("second page: %.200s", out)
	}
}
