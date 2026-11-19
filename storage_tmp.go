package main

import (
	"context"
	"errors"
	"io"
	"os"
)

// TmpStorageBackend implements StorageBackend for ephemeral tmp-only mode where
// streams buffer through TMP_DIR without touching or creating CACHE_DIR.
type TmpStorageBackend struct {
	tmpDir string
}

// NewTmpStorageBackend creates an ephemeral tmp-only storage backend.
func NewTmpStorageBackend(tmpDir string) *TmpStorageBackend {
	return &TmpStorageBackend{tmpDir: tmpDir}
}

func (b *TmpStorageBackend) Type() string {
	return "tmp"
}

func (b *TmpStorageBackend) Stat(ctx context.Context, key string) (*FileMeta, bool, error) {
	return nil, false, nil
}

func (b *TmpStorageBackend) Open(ctx context.Context, key string) (StorageReadSeeker, error) {
	return nil, os.ErrNotExist
}

func (b *TmpStorageBackend) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}

func (b *TmpStorageBackend) HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error) {
	return false, nil
}

func (b *TmpStorageBackend) OpenChunkRange(ctx context.Context, key string, chunkIndex int, offset, length int64) (io.ReadCloser, error) {
	return nil, errors.New("chunks not supported on tmp backend")
}

func (b *TmpStorageBackend) UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error {
	return errors.New("chunk uploads not supported on tmp backend")
}

func (b *TmpStorageBackend) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	return errors.New("recomposition not supported on tmp backend")
}

func (b *TmpStorageBackend) CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error {
	if localPartPath != "" {
		_ = os.Remove(localPartPath)
	}
	return nil
}

func (b *TmpStorageBackend) Delete(ctx context.Context, key string) error {
	return nil
}

func (b *TmpStorageBackend) UpdateMetadata(ctx context.Context, key string, meta FileMeta) error {
	return nil
}

func (b *TmpStorageBackend) List(ctx context.Context) ([]StorageObjectInfo, error) {
	return nil, nil
}
