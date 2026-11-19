package main

import (
	"context"
	"io"
	"time"
)

// StorageObjectInfo describes a completed asset stored in a StorageBackend.
type StorageObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// StorageReadSeeker extends io.ReadSeeker with io.ReaderAt, io.Closer, and Size()
type StorageReadSeeker interface {
	io.Reader
	io.Seeker
	io.ReaderAt
	io.Closer
	Size() int64
}

// StorageTier identifies which tier a media file is currently served from or routed to.
type StorageTier string

const (
	TierDisk  StorageTier = "disk"
	TierS3    StorageTier = "s3"
	TierProbe StorageTier = "probe"
)

// ChunkCleanupBackend is optionally implemented by remote chunked backends (such as S3)
// so callers can clean up staging chunks on failure, cancellation, or eviction.
type ChunkCleanupBackend interface {
	DeleteChunks(ctx context.Context, key string) error
	SweepStaleChunks(ctx context.Context, maxAge time.Duration) (int, error)
}

// StorageBackend defines the interface for persisting and serving media cache assets
type StorageBackend interface {
	// Type returns the storage backend identifier (e.g., "disk", "s3")
	Type() string

	// Stat checks whether an asset exists in storage and returns its metadata and completion status
	Stat(ctx context.Context, key string) (*FileMeta, bool, error)

	// Open returns a StorageReadSeeker for reading the committed monolithic file
	Open(ctx context.Context, key string) (StorageReadSeeker, error)

	// OpenRange opens a reader for a specific byte range [offset, offset+length)
	// If length <= 0, it reads from offset to EOF
	OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// HasChunk checks whether a specific chunk is committed in storage
	HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error)

	// OpenChunkRange opens a reader for a byte range within a specific temporary chunk
	OpenChunkRange(ctx context.Context, key string, chunkIndex int, offset, length int64) (io.ReadCloser, error)

	// UploadChunk stores an individual chunk into storage (Tier 2)
	UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error

	// Recompose stitches uploaded chunks into the final monolithic object server-side
	Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error

	// CommitLocalFile finalizes a local .part file into the storage backend
	CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error

	// Delete removes the asset (including chunks and metadata) from storage
	Delete(ctx context.Context, key string) error

	// UpdateMetadata refreshes the FileMeta (e.g. LastValidated) in storage
	UpdateMetadata(ctx context.Context, key string, meta FileMeta) error

	// List returns all completed monolithic assets tracked by the storage backend
	List(ctx context.Context) ([]StorageObjectInfo, error)
}

// CommitProgress describes the active commit/recomposition phase and item progress.
type CommitProgress struct {
	Stage     string `json:"stage"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

type commitProgressCtxKey struct{}

// CommitProgressFunc receives (stage, completed, total) updates during S3 commit/recompose.
type CommitProgressFunc func(stage string, done, total int)

// WithCommitProgress attaches a progress callback to ctx for StorageBackend implementations.
func WithCommitProgress(ctx context.Context, fn CommitProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, commitProgressCtxKey{}, fn)
}

func commitProgressFromContext(ctx context.Context) CommitProgressFunc {
	if ctx == nil {
		return nil
	}
	if fn, ok := ctx.Value(commitProgressCtxKey{}).(CommitProgressFunc); ok {
		return fn
	}
	return nil
}

// GlobalStorage holds the active singleton storage backend
var GlobalStorage StorageBackend

// InitStorage initializes the global storage backend according to configuration
func InitStorage() error {
	backendType := GlobalConfig.GetStorageBackend()
	if backendType == "s3" {
		s3Backend, err := NewS3StorageBackend(GlobalConfig.GetS3Config())
		if err != nil {
			return err
		}
		GlobalStorage = s3Backend
		return nil
	}

	GlobalStorage = NewLocalStorageBackend(GlobalConfig.GetCacheDir(), GlobalConfig.GetTmpDir())
	return nil
}
