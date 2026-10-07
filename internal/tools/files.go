package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// FileInfo describes a stored file.
type FileInfo struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FilesBackend is one tenant's file storage, already bound to the tenant
// and its quota (see tenant.filesBackend).
type FilesBackend interface {
	List(ctx context.Context, prefix string) ([]FileInfo, error)
	Read(ctx context.Context, path string) (FileInfo, []byte, error)
	Write(ctx context.Context, path string, data []byte) (FileInfo, error)
	Delete(ctx context.Context, path string) error
}

// Files exposes a tenant's file storage to its agents: list, read, write,
// delete. Text only for reading and writing; binary files (uploaded PDFs,
// images) are listed and can be deleted, and read reports their type.
type Files struct {
	name string
	b    FilesBackend
}

func NewFiles(name string, b FilesBackend) *Files { return &Files{name: name, b: b} }

func (f *Files) Name() string { return f.name }
func (f *Files) Close() error { return nil }

const maxReadChars = 50_000

func (f *Files) Tools(context.Context) ([]Tool, error) {
	return []Tool{
		{
			Local:       "list",
			Description: "Lists stored files (path, size, type, last change). Folders are path prefixes such as \"reports/\".",
			Schema:      Schema(map[string]any{"prefix": Prop("string", "only files whose path starts with this, e.g. \"reports/\"; empty lists all")}),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Prefix string `json:"prefix"`
				}
				if err := Decode(args, &a); err != nil {
					return "", err
				}
				list, err := f.b.List(ctx, a.Prefix)
				if err != nil {
					return "", err
				}
				return JSON(map[string]any{"files": list, "count": len(list)}), nil
			},
		},
		{
			Local:       "read",
			Description: "Reads a text file (txt, md, csv, json, ...). Long files are returned in pieces: pass offset to continue.",
			Schema: Schema(map[string]any{
				"path":   Prop("string", "file path, e.g. \"notes/customers.md\""),
				"offset": Prop("integer", "character offset to start from, default 0"),
			}, "path"),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path   string `json:"path"`
					Offset int    `json:"offset"`
				}
				if err := Decode(args, &a); err != nil {
					return "", err
				}
				info, data, err := f.b.Read(ctx, a.Path)
				if err != nil {
					return "", err
				}
				if !utf8.Valid(data) {
					return "", fmt.Errorf("%s is a binary file (%s, %d bytes) and cannot be read as text", info.Path, info.ContentType, info.Size)
				}
				runes := []rune(string(data))
				if a.Offset < 0 || a.Offset > len(runes) {
					a.Offset = 0
				}
				end := min(a.Offset+maxReadChars, len(runes))
				out := map[string]any{"path": info.Path, "content": string(runes[a.Offset:end]), "total_chars": len(runes)}
				if end < len(runes) {
					out["next_offset"] = end
				}
				return JSON(out), nil
			},
		},
		{
			Local:       "write",
			Description: "Creates or overwrites a text file. Set append to add to the end of an existing file instead.",
			Schema: Schema(map[string]any{
				"path":    Prop("string", "file path, e.g. \"reports/2026-09.md\"; folders are created implicitly"),
				"content": Prop("string", "the text to write"),
				"append":  Prop("boolean", "append to the existing file instead of replacing it"),
			}, "path", "content"),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path    string `json:"path"`
					Content string `json:"content"`
					Append  bool   `json:"append"`
				}
				if err := Decode(args, &a); err != nil {
					return "", err
				}
				data := []byte(a.Content)
				if a.Append {
					if _, old, err := f.b.Read(ctx, a.Path); err == nil {
						data = append(old, data...)
					}
				}
				info, err := f.b.Write(ctx, a.Path, data)
				if err != nil {
					return "", err
				}
				return JSON(map[string]any{"written": info.Path, "size": info.Size}), nil
			},
		},
		{
			Local:       "delete",
			Description: "Deletes a file.",
			Schema:      Schema(map[string]any{"path": Prop("string", "file path")}, "path"),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path string `json:"path"`
				}
				if err := Decode(args, &a); err != nil {
					return "", err
				}
				if err := f.b.Delete(ctx, a.Path); err != nil {
					return "", err
				}
				return JSON(map[string]any{"deleted": a.Path}), nil
			},
		},
	}, nil
}
