package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LocalStorageBackend implements StorageBackend using the local filesystem
type LocalStorageBackend struct {
	cacheDir string
	tmpDir   string
	mu       sync.RWMutex
}

// NewLocalStorageBackend initializes a local filesystem storage backend
func NewLocalStorageBackend(cacheDir, tmpDir string) *LocalStorageBackend {
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)
	return &LocalStorageBackend{
		cacheDir: cacheDir,
		tmpDir:   tmpDir,
	}
}

func (b *LocalStorageBackend) Type() string {
	return "local"
}

func (b *LocalStorageBackend) Stat(ctx context.Context, key string) (*FileMeta, bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	filePath := filepath.Join(b.cacheDir, key)
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	metaPath := filePath + ".json"
	metaBytes, err := os.ReadFile(metaPath)
	if err == nil {
		var meta FileMeta
		if err := json.Unmarshal(metaBytes, &meta); err == nil {
			return &meta, true, nil
		}
	}

	// Fallback metadata if .json is missing
	meta := FileMeta{
		TotalSize:     info.Size(),
		LastValidated: info.ModTime().Unix(),
		Completed:     true,
	}
	return &meta, true, nil
}

type localFileReadSeeker struct {
	file *os.File
	size int64
}

func (r *localFileReadSeeker) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *localFileReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.file.Seek(offset, whence)
}

func (r *localFileReadSeeker) ReadAt(p []byte, off int64) (int, error) {
	return r.file.ReadAt(p, off)
}

func (r *localFileReadSeeker) Close() error {
	if r.file != nil {
		FadviseDontNeed(r.file.Fd(), 0, 0)
		return r.file.Close()
	}
	return nil
}

func (r *localFileReadSeeker) Size() int64 {
	return r.size
}

func (b *LocalStorageBackend) Open(ctx context.Context, key string) (StorageReadSeeker, error) {
	b.mu.RLock()
	filePath := filepath.Join(b.cacheDir, key)
	b.mu.RUnlock()

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return &localFileReadSeeker{
		file: f,
		size: info.Size(),
	}, nil
}

type rangeReadCloser struct {
	file   *os.File
	reader io.Reader
}

func (r *rangeReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *rangeReadCloser) Close() error {
	if r.file != nil {
		FadviseDontNeed(r.file.Fd(), 0, 0)
		return r.file.Close()
	}
	return nil
}

func (b *LocalStorageBackend) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	b.mu.RLock()
	filePath := filepath.Join(b.cacheDir, key)
	b.mu.RUnlock()

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	var r io.Reader = f
	if length > 0 {
		r = io.LimitReader(f, length)
	}

	return &rangeReadCloser{
		file:   f,
		reader: r,
	}, nil
}

func (b *LocalStorageBackend) HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error) {
	return false, nil
}

func (b *LocalStorageBackend) OpenChunkRange(ctx context.Context, key string, chunkIndex int, offset, length int64) (io.ReadCloser, error) {
	return nil, errors.New("chunks not supported on local backend")
}

func (b *LocalStorageBackend) UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error {
	return errors.New("chunk uploads not supported on local backend")
}

func (b *LocalStorageBackend) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	return errors.New("recomposition not supported on local backend")
}

func (b *LocalStorageBackend) CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	targetPath := filepath.Join(b.cacheDir, key)
	if err := os.Rename(localPartPath, targetPath); err != nil {
		return err
	}

	// Evict clean pages from host OS cache upon rename
	if cf, err := os.Open(targetPath); err == nil {
		FadviseDontNeed(cf.Fd(), 0, 0)
		_ = cf.Close()
	}

	meta.Completed = true
	meta.LastValidated = time.Now().Unix()
	metaBytes, err := json.Marshal(meta)
	if err == nil {
		_ = os.WriteFile(targetPath+".json", metaBytes, 0644)
	}

	return nil
}

func (b *LocalStorageBackend) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	filePath := filepath.Join(b.cacheDir, key)
	_ = os.Remove(filePath)
	_ = os.Remove(filePath + ".json")
	return nil
}

func (b *LocalStorageBackend) UpdateMetadata(ctx context.Context, key string, meta FileMeta) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	targetPath := filepath.Join(b.cacheDir, key)
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath+".json", metaBytes, 0644)
}
