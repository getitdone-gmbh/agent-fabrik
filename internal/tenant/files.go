package tenant

import (
	"context"

	"github.com/getitdone-gmbh/agentd/internal/store"
	"github.com/getitdone-gmbh/agentd/internal/tools"
)

// Files is one tenant's file storage with its plan quota applied. The agent
// tools (files.*) and the /v1/files API both go through it, so paths are
// validated and the quota is enforced in one place.
type Files struct {
	st     store.FileStore
	tenant string
	quota  store.FileQuota
	slots  chan struct{}
}

// maxFileTransfers is how many whole files a replica holds in memory at
// once. Files live in Postgres and are read and written in one piece, so
// without a bound a few parallel 25 MB uploads exceed the pod's memory.
const maxFileTransfers = 2

// Hold waits for one of the replica's file-transfer slots. Callers that
// read or receive a whole file hold it until the bytes are gone.
func (f *Files) Hold(ctx context.Context) (func(), error) {
	select {
	case f.slots <- struct{}{}:
		return func() { <-f.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Files returns the tenant's file storage.
func (m *Manager) Files(ctx context.Context, tenant string) (*Files, error) {
	t, err := m.Tenant(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return m.filesFor(t), nil
}

func (m *Manager) filesFor(t *store.Tenant) *Files {
	files, fileBytes, total := m.Limits(t).FileLimits()
	return &Files{st: m.st, tenant: t.ID, slots: m.fileSlots, quota: store.FileQuota{MaxFiles: files, MaxFileBytes: fileBytes, MaxTotalBytes: total}}
}

// Quota reports the limits (0 = unlimited).
func (f *Files) Quota() store.FileQuota { return f.quota }

func (f *Files) Usage(ctx context.Context) (store.FileUsage, error) {
	return f.st.FileUsage(ctx, f.tenant)
}

func (f *Files) Put(ctx context.Context, path string, data []byte, contentType string) (store.File, error) {
	p, err := store.CleanFilePath(path)
	if err != nil {
		return store.File{}, badRequest(err)
	}
	if contentType == "" {
		contentType = store.ContentTypeFor(p)
	}
	return f.st.PutFile(ctx, f.tenant, p, data, contentType, f.quota)
}

func (f *Files) Get(ctx context.Context, path string) (store.File, []byte, error) {
	p, err := store.CleanFilePath(path)
	if err != nil {
		return store.File{}, nil, badRequest(err)
	}
	return f.st.GetFile(ctx, f.tenant, p)
}

func (f *Files) ListFiles(ctx context.Context, prefix string, limit int) ([]store.File, error) {
	p, err := store.CleanFilePrefix(prefix)
	if err != nil {
		return nil, badRequest(err)
	}
	return f.st.ListFiles(ctx, f.tenant, p, limit)
}

func (f *Files) Remove(ctx context.Context, path string) error {
	p, err := store.CleanFilePath(path)
	if err != nil {
		return badRequest(err)
	}
	return f.st.DeleteFile(ctx, f.tenant, p)
}

// ----- tools.FilesBackend -----

func info(f store.File) tools.FileInfo {
	return tools.FileInfo{Path: f.Path, Size: f.Size, ContentType: f.ContentType, UpdatedAt: f.UpdatedAt}
}

func (f *Files) List(ctx context.Context, prefix string) ([]tools.FileInfo, error) {
	list, err := f.ListFiles(ctx, prefix, 500)
	if err != nil {
		return nil, err
	}
	out := make([]tools.FileInfo, len(list))
	for i, x := range list {
		out[i] = info(x)
	}
	return out, nil
}

func (f *Files) Read(ctx context.Context, path string) (tools.FileInfo, []byte, error) {
	release, err := f.Hold(ctx)
	if err != nil {
		return tools.FileInfo{}, nil, err
	}
	defer release()
	x, data, err := f.Get(ctx, path)
	if err != nil {
		if err == store.ErrNotFound {
			return tools.FileInfo{}, nil, notFoundFile(path)
		}
		return tools.FileInfo{}, nil, err
	}
	return info(x), data, nil
}

func (f *Files) Write(ctx context.Context, path string, data []byte) (tools.FileInfo, error) {
	x, err := f.Put(ctx, path, data, "")
	return info(x), err
}

func (f *Files) Delete(ctx context.Context, path string) error {
	if err := f.Remove(ctx, path); err != nil {
		if err == store.ErrNotFound {
			return notFoundFile(path)
		}
		return err
	}
	return nil
}

func badRequest(err error) error { return &ValidationError{err.Error()} }

func notFoundFile(path string) error { return &ValidationError{"file not found: " + path} }
