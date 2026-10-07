package store

import (
	"context"
	"errors"
	"testing"
)

func TestCleanFilePath(t *testing.T) {
	ok := map[string]string{
		"a.txt": "a.txt", "reports/2026-09.md": "reports/2026-09.md", "a//b.txt": "a/b.txt",
		"./a.txt": "a.txt", "Kunden/Müller & Söhne.csv": "Kunden/Müller & Söhne.csv",
	}
	for in, want := range ok {
		if got, err := CleanFilePath(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "a/..", `a\\b`, "a/", ".", "a/\x00b", " x/ y"} {
		if got, err := CleanFilePath(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestFilesConformance(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			defer s.Close()
			filesConformance(t, s)
		})
	}
}

func filesConformance(t *testing.T, s Store) {
	ctx := context.Background()
	s.CreateTenant(ctx, Tenant{ID: "f1"})
	s.CreateTenant(ctx, Tenant{ID: "f2"})
	q := FileQuota{MaxFiles: 2, MaxFileBytes: 10, MaxTotalBytes: 14}

	if _, err := s.PutFile(ctx, "nope", "a.txt", []byte("x"), "text/plain", q); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
	f, err := s.PutFile(ctx, "f1", "docs/a.txt", []byte("hello"), "text/plain", q)
	if err != nil || f.Size != 5 || f.UpdatedAt.IsZero() {
		t.Fatalf("put: %+v %v", f, err)
	}
	if _, err := s.PutFile(ctx, "f1", "docs/big.txt", []byte("12345678901"), "text/plain", q); !errors.Is(err, ErrFileQuota) {
		t.Fatalf("per-file limit: %v", err)
	}
	if _, err := s.PutFile(ctx, "f1", "b.txt", []byte("1234567890"), "text/plain", q); !errors.Is(err, ErrFileQuota) {
		t.Fatalf("total limit: %v", err)
	}
	if _, err := s.PutFile(ctx, "f1", "b.txt", []byte("12345678"), "text/plain", q); err != nil {
		t.Fatalf("within quota: %v", err)
	}
	if _, err := s.PutFile(ctx, "f1", "c.txt", []byte("1"), "text/plain", q); !errors.Is(err, ErrFileQuota) {
		t.Fatalf("file count: %v", err)
	}
	// Replacing does not count the old version twice.
	if _, err := s.PutFile(ctx, "f1", "docs/a.txt", []byte("hello!"), "text/markdown", q); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, data, err := s.GetFile(ctx, "f1", "docs/a.txt")
	if err != nil || string(data) != "hello!" || got.ContentType != "text/markdown" {
		t.Fatalf("get: %+v %q %v", got, data, err)
	}
	// Tenants are separate.
	if _, _, err := s.GetFile(ctx, "f2", "docs/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("f2 reads f1: %v", err)
	}
	list, _ := s.ListFiles(ctx, "f1", "docs/", 0)
	all, _ := s.ListFiles(ctx, "f1", "", 0)
	if len(list) != 1 || list[0].Path != "docs/a.txt" || len(all) != 2 || all[0].Path != "b.txt" {
		t.Fatalf("list: %+v / %+v", list, all)
	}
	// "_" and "%" in a prefix are literal, not LIKE wildcards.
	if l, _ := s.ListFiles(ctx, "f1", "doc_", 0); len(l) != 0 {
		t.Fatalf("LIKE wildcard leaked: %+v", l)
	}
	u, _ := s.FileUsage(ctx, "f1")
	if u.Files != 2 || u.Bytes != 14 {
		t.Fatalf("usage: %+v", u)
	}
	if err := s.DeleteFile(ctx, "f1", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile(ctx, "f1", "b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Files go with the tenant.
	if err := s.DeleteTenant(ctx, "f1"); err != nil {
		t.Fatal(err)
	}
	s.CreateTenant(ctx, Tenant{ID: "f1"})
	if l, _ := s.ListFiles(ctx, "f1", "", 0); len(l) != 0 {
		t.Fatalf("files survived tenant deletion: %+v", l)
	}
}
