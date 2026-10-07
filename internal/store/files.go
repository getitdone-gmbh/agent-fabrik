package store

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// File is a stored file's metadata.
type File struct {
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// FileUsage is what a tenant's files take up.
type FileUsage struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// FileQuota caps a tenant's files. Zero means unlimited.
type FileQuota struct {
	MaxFiles      int64
	MaxFileBytes  int64
	MaxTotalBytes int64
}

// ErrFileQuota is returned when a write would exceed the tenant's quota.
var ErrFileQuota = errors.New("file quota exceeded")

// FileStore keeps each tenant's files (agent working files, uploads). Paths
// are relative, slash-separated and validated with CleanFilePath.
type FileStore interface {
	// PutFile creates or replaces a file; the quota is checked atomically
	// against the tenant's other files.
	PutFile(ctx context.Context, tenant, path string, data []byte, contentType string, q FileQuota) (File, error)
	GetFile(ctx context.Context, tenant, path string) (File, []byte, error)
	// ListFiles returns files whose path starts with prefix, sorted by path.
	ListFiles(ctx context.Context, tenant, prefix string, limit int) ([]File, error)
	DeleteFile(ctx context.Context, tenant, path string) error
	FileUsage(ctx context.Context, tenant string) (FileUsage, error)
}

const (
	maxPathLen   = 512
	maxPathDepth = 16
)

// CleanFilePath validates a tenant file path and returns its canonical form
// ("a/b.txt"). Rejected: absolute paths, "..", empty segments after
// cleaning, backslashes, control characters, overlong paths.
func CleanFilePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is empty")
	}
	if !utf8.ValidString(p) || len(p) > maxPathLen {
		return "", fmt.Errorf("path must be valid UTF-8 and at most %d bytes", maxPathLen)
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", errors.New("path must be relative and use / as separator")
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return "", errors.New("path contains control characters")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errors.New("path must not contain ..")
		}
	}
	c := path.Clean(p)
	if c == "." || strings.HasSuffix(p, "/") {
		return "", errors.New("path must name a file")
	}
	if strings.Count(c, "/") >= maxPathDepth {
		return "", fmt.Errorf("path is nested deeper than %d levels", maxPathDepth)
	}
	for _, seg := range strings.Split(c, "/") {
		if strings.TrimSpace(seg) != seg {
			return "", errors.New("path segments must not start or end with spaces")
		}
	}
	return c, nil
}

// CleanFilePrefix validates a listing prefix ("" lists everything).
func CleanFilePrefix(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return "", nil
	}
	dir := strings.HasSuffix(p, "/")
	c, err := CleanFilePath(strings.TrimSuffix(p, "/"))
	if err != nil {
		return "", err
	}
	if dir {
		c += "/"
	}
	return c, nil
}

// ContentTypeFor guesses a content type from the file name.
func ContentTypeFor(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".txt", ".log", "":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// checkQuota validates a write given the tenant's usage WITHOUT the file
// being replaced.
func checkQuota(q FileQuota, others FileUsage, size int64) error {
	switch {
	case q.MaxFileBytes > 0 && size > q.MaxFileBytes:
		return fmt.Errorf("%w: file is %d bytes, limit is %d per file", ErrFileQuota, size, q.MaxFileBytes)
	case q.MaxFiles > 0 && others.Files+1 > q.MaxFiles:
		return fmt.Errorf("%w: limit is %d files", ErrFileQuota, q.MaxFiles)
	case q.MaxTotalBytes > 0 && others.Bytes+size > q.MaxTotalBytes:
		return fmt.Errorf("%w: storage limit is %d bytes, %d in use", ErrFileQuota, q.MaxTotalBytes, others.Bytes)
	}
	return nil
}
