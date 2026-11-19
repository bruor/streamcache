package main

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// TieredStorageBackend composes an L1 local disk/NAS backend with an L2 S3 backend
// for adaptive hybrid caching, serving from local L1 first and falling back to S3 L2.
type TieredStorageBackend struct {
	local *LocalStorageBackend
	s3    *S3StorageBackend
}

// NewTieredStorageBackend creates a hybrid storage backend pairing local L1 and S3 L2.
func NewTieredStorageBackend(local *LocalStorageBackend, s3Backend *S3StorageBackend) *TieredStorageBackend {
	return &TieredStorageBackend{
		local: local,
		s3:    s3Backend,
	}
}

func (b *TieredStorageBackend) Local() *LocalStorageBackend {
	if b == nil {
		return nil
	}
	return b.local
}

func (b *TieredStorageBackend) S3() *S3StorageBackend {
	if b == nil {
		return nil
	}
	return b.s3
}

func (b *TieredStorageBackend) Type() string {
	return "hybrid"
}

func (b *TieredStorageBackend) Stat(ctx context.Context, key string) (*FileMeta, bool, error) {
	meta, _, exists, err := b.StatWithTier(ctx, key)
	return meta, exists, err
}

func (b *TieredStorageBackend) StatWithTier(ctx context.Context, key string) (*FileMeta, StorageTier, bool, error) {
	var localErr error
	if b.local != nil && IsNASHealthy() {
		meta, tier, exists, err := b.local.StatWithTier(ctx, key)
		if err == nil {
			RecordNASSuccess()
			if exists {
				return meta, tier, true, nil
			}
		} else {
			RecordNASFailure(err)
			localErr = err
		}
	}

	if b.s3 != nil && IsS3Healthy() {
		meta, tier, exists, err := b.s3.StatWithTier(ctx, key)
		if err == nil && exists {
			return meta, tier, true, nil
		}
		if err != nil {
			return nil, "", false, err
		}
	}

	return nil, "", false, localErr
}

func (b *TieredStorageBackend) Open(ctx context.Context, key string) (StorageReadSeeker, error) {
	return b.OpenWithMeta(ctx, key, nil, "")
}

func (b *TieredStorageBackend) OpenWithMeta(ctx context.Context, key string, meta *FileMeta, tier StorageTier) (StorageReadSeeker, error) {
	switch tier {
	case TierS3:
		if b.s3 == nil {
			return nil, os.ErrNotExist
		}
		if !IsS3Healthy() {
			return nil, ErrStorageCircuitOpen
		}
		return b.s3.OpenWithMeta(ctx, key, meta, TierS3)
	case TierDisk:
		if b.local != nil && IsNASHealthy() {
			r, err := b.local.Open(ctx, key)
			if err == nil {
				RecordNASSuccess()
				return r, nil
			}
			if !os.IsNotExist(err) {
				RecordNASFailure(err)
			}
			if b.s3 != nil && IsS3Healthy() {
				return b.s3.Open(ctx, key)
			}
			return nil, err
		}
		if b.s3 != nil && IsS3Healthy() {
			return b.s3.Open(ctx, key)
		}
		if !IsNASHealthy() {
			return nil, ErrStorageCircuitOpen
		}
		return nil, os.ErrNotExist
	default:
		if b.local != nil && IsNASHealthy() {
			r, err := b.local.Open(ctx, key)
			if err == nil {
				RecordNASSuccess()
				return r, nil
			}
			if !os.IsNotExist(err) {
				RecordNASFailure(err)
			}
		}
		if b.s3 != nil && IsS3Healthy() {
			return b.s3.OpenWithMeta(ctx, key, meta, TierS3)
		}
		if (b.local != nil && !IsNASHealthy()) || (b.s3 != nil && !IsS3Healthy()) {
			return nil, ErrStorageCircuitOpen
		}
		return nil, os.ErrNotExist
	}
}

func (b *TieredStorageBackend) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if b.local != nil && IsNASHealthy() {
		rc, err := b.local.OpenRange(ctx, key, offset, length)
		if err == nil {
			RecordNASSuccess()
			return rc, nil
		}
		if !os.IsNotExist(err) {
			RecordNASFailure(err)
		}
		if b.s3 == nil || !IsS3Healthy() {
			return nil, err
		}
	}
	if b.s3 != nil && IsS3Healthy() {
		return b.s3.OpenRange(ctx, key, offset, length)
	}
	if (b.local != nil && !IsNASHealthy()) || (b.s3 != nil && !IsS3Healthy()) {
		return nil, ErrStorageCircuitOpen
	}
	return nil, os.ErrNotExist
}

func (b *TieredStorageBackend) HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error) {
	if b.s3 == nil {
		return false, nil
	}
	return b.s3.HasChunk(ctx, key, chunkIndex)
}

func (b *TieredStorageBackend) OpenChunkRange(ctx context.Context, key string, chunkIndex int, offset, length int64) (io.ReadCloser, error) {
	if b.s3 == nil {
		return nil, errors.New("s3 backend not configured")
	}
	return b.s3.OpenChunkRange(ctx, key, chunkIndex, offset, length)
}

func (b *TieredStorageBackend) UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error {
	if b.s3 == nil {
		return errors.New("s3 backend not configured")
	}
	return b.s3.UploadChunk(ctx, key, chunkIndex, r, size)
}

func (b *TieredStorageBackend) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	if b.s3 == nil {
		return errors.New("s3 backend not configured")
	}
	return b.s3.Recompose(ctx, key, totalParts, meta)
}

func (b *TieredStorageBackend) DeleteChunks(ctx context.Context, key string) error {
	if b.s3 == nil {
		return nil
	}
	return b.s3.DeleteChunks(ctx, key)
}

func (b *TieredStorageBackend) SweepStaleChunks(ctx context.Context, maxAge time.Duration) (int, error) {
	if b.s3 == nil {
		return 0, nil
	}
	return b.s3.SweepStaleChunks(ctx, maxAge)
}

func (b *TieredStorageBackend) CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error {
	if b.local == nil {
		return errors.New("local backend not configured")
	}
	return b.local.CommitLocalFile(ctx, key, localPartPath, meta)
}

func (b *TieredStorageBackend) Delete(ctx context.Context, key string) error {
	var firstErr error
	if b.local != nil {
		if err := b.local.Delete(ctx, key); err != nil && !os.IsNotExist(err) {
			firstErr = err
		}
	}
	if b.s3 != nil {
		if err := b.s3.Delete(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *TieredStorageBackend) DeleteLocal(ctx context.Context, key string) error {
	if b.local == nil {
		return nil
	}
	return b.local.Delete(ctx, key)
}

func (b *TieredStorageBackend) DeleteS3(ctx context.Context, key string) error {
	if b.s3 == nil {
		return nil
	}
	return b.s3.Delete(ctx, key)
}

// UpdateMetadataForTier refreshes metadata on a specific tier (or both present tiers when tier is "").
func (b *TieredStorageBackend) UpdateMetadataForTier(ctx context.Context, key string, meta FileMeta, tier StorageTier) error {
	switch tier {
	case TierDisk:
		if b.local != nil {
			return b.local.UpdateMetadata(ctx, key, meta)
		}
		return nil
	case TierS3:
		if b.s3 != nil {
			return b.s3.UpdateMetadata(ctx, key, meta)
		}
		return nil
	default:
		var firstErr error
		if b.local != nil {
			if _, exists, err := b.local.Stat(ctx, key); err == nil && exists {
				if err := b.local.UpdateMetadata(ctx, key, meta); err != nil {
					firstErr = err
				}
			}
		}
		if b.s3 != nil {
			if _, exists, err := b.s3.Stat(ctx, key); err == nil && exists {
				if err := b.s3.UpdateMetadata(ctx, key, meta); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}
}

func (b *TieredStorageBackend) UpdateMetadata(ctx context.Context, key string, meta FileMeta) error {
	return b.UpdateMetadataForTier(ctx, key, meta, "")
}

func (b *TieredStorageBackend) List(ctx context.Context) ([]StorageObjectInfo, error) {
	seen := make(map[string]bool)
	var merged []StorageObjectInfo

	if b.local != nil {
		localObjs, err := b.local.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range localObjs {
			if !seen[obj.Key] {
				seen[obj.Key] = true
				merged = append(merged, obj)
			}
		}
	}

	if b.s3 != nil {
		s3Objs, err := b.s3.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range s3Objs {
			if !seen[obj.Key] {
				seen[obj.Key] = true
				merged = append(merged, obj)
			}
		}
	}

	return merged, nil
}
