package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	defaultS3ReadAheadSize = 2 * 1024 * 1024 // 2MB read-ahead buffer
)

// S3StorageBackend implements StorageBackend using any S3-compatible service
type S3StorageBackend struct {
	client               *s3.Client
	bucket               string
	chunkSize            int64
	uploadConcurrency    int
	recomposeConcurrency int
	mu                   sync.RWMutex
	metaCache            map[string]FileMeta
}

// NewS3StorageBackend initializes an S3StorageBackend from S3Config
func NewS3StorageBackend(cfg S3Config) (*S3StorageBackend, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3 bucket must not be empty")
	}

	var baseEndpoint *string
	if cfg.Endpoint != "" {
		endpoint := cfg.Endpoint
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			if cfg.DisableSSL {
				endpoint = "http://" + endpoint
			} else {
				endpoint = "https://" + endpoint
			}
		}
		baseEndpoint = aws.String(endpoint)
	}

	creds := credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: baseEndpoint,
		UsePathStyle: cfg.UsePathStyle,
		Credentials:  creds,
	})

	chunkSize := cfg.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 64 * 1024 * 1024
	}

	uploadConc := clampConcurrency(cfg.UploadConcurrency, 4)
	recomposeConc := clampConcurrency(cfg.RecomposeConcurrency, 6)

	return &S3StorageBackend{
		client:               client,
		bucket:               cfg.Bucket,
		chunkSize:            chunkSize,
		uploadConcurrency:    uploadConc,
		recomposeConcurrency: recomposeConc,
		metaCache:            make(map[string]FileMeta),
	}, nil
}

func (b *S3StorageBackend) ChunkSize() int64 {
	return b.chunkSize
}

func (b *S3StorageBackend) UploadConcurrency() int {
	return b.uploadConcurrency
}

func (b *S3StorageBackend) RecomposeConcurrency() int {
	return b.recomposeConcurrency
}

func (b *S3StorageBackend) Type() string {
	return "s3"
}

func (b *S3StorageBackend) monolithicKey(key string) string {
	return "vod/" + key
}

func (b *S3StorageBackend) chunkKey(key string, chunkIndex int) string {
	return fmt.Sprintf("chunks/%s/chunk_%05d.bin", key, chunkIndex)
}

func (b *S3StorageBackend) metaToMap(meta FileMeta) map[string]string {
	m := make(map[string]string)
	if meta.URL != "" {
		m["upstream-url"] = meta.URL
	}
	if meta.TotalSize > 0 {
		m["total-size"] = strconv.FormatInt(meta.TotalSize, 10)
	}
	if meta.ContentType != "" {
		m["content-type"] = meta.ContentType
	}
	if meta.ETag != "" {
		m["etag"] = meta.ETag
	}
	if meta.LastModified != "" {
		m["last-modified"] = meta.LastModified
	}
	if meta.LastValidated > 0 {
		m["last-validated"] = strconv.FormatInt(meta.LastValidated, 10)
	}
	return m
}

func (b *S3StorageBackend) mapToMeta(m map[string]string) FileMeta {
	var meta FileMeta
	if m == nil {
		return meta
	}
	for k, v := range m {
		kLower := strings.ToLower(k)
		switch {
		case strings.HasSuffix(kLower, "upstream-url"):
			meta.URL = v
		case strings.HasSuffix(kLower, "total-size"):
			if s, err := strconv.ParseInt(v, 10, 64); err == nil {
				meta.TotalSize = s
			}
		case strings.HasSuffix(kLower, "content-type"):
			meta.ContentType = v
		case strings.HasSuffix(kLower, "etag"):
			meta.ETag = v
		case strings.HasSuffix(kLower, "last-modified"):
			meta.LastModified = v
		case strings.HasSuffix(kLower, "last-validated"):
			if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
				meta.LastValidated = ts
			}
		}
	}
	return meta
}

func (b *S3StorageBackend) Stat(ctx context.Context, key string) (*FileMeta, bool, error) {
	meta, _, exists, err := b.StatWithTier(ctx, key)
	return meta, exists, err
}

func isS3NotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var nfe *types.NoSuchKey
	var notFound *types.NotFound
	return errors.As(err, &nfe) || errors.As(err, &notFound) ||
		strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound")
}

func recordS3Outcome(err error) error {
	if err != nil {
		RecordS3Failure(err)
		return err
	}
	RecordS3Success()
	return nil
}

// Ping executes a lightweight ListObjectsV2 check to verify S3 reachability and update circuit state.
func (b *S3StorageBackend) Ping(ctx context.Context) error {
	if b == nil || b.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := b.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String("vod/"),
		MaxKeys: aws.Int32(1),
	})
	return recordS3Outcome(err)
}

func (b *S3StorageBackend) StatWithTier(ctx context.Context, key string) (*FileMeta, StorageTier, bool, error) {
	targetKey := b.monolithicKey(key)
	resp, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(targetKey),
	})
	if err != nil {
		if isS3NotFoundError(err) {
			RecordS3Success()
			return nil, "", false, nil
		}
		RecordS3Failure(err)
		return nil, "", false, err
	}
	RecordS3Success()

	meta := b.mapToMeta(resp.Metadata)
	if meta.TotalSize == 0 && resp.ContentLength != nil {
		meta.TotalSize = *resp.ContentLength
	}
	meta.Completed = true

	b.mu.RLock()
	if b.metaCache != nil {
		if mc, ok := b.metaCache[key]; ok {
			b.mu.RUnlock()
			if mc.TotalSize <= 0 && meta.TotalSize > 0 {
				mc.TotalSize = meta.TotalSize
			}
			mc.Completed = true
			return &mc, TierS3, true, nil
		}
	}
	b.mu.RUnlock()

	if idx := getGlobalCacheIndex(); idx != nil {
		if entry, ok := idx.Get(key); ok {
			if entry.LastValidated > meta.LastValidated {
				meta.LastValidated = entry.LastValidated
			}
			if entry.ETag != "" && meta.ETag == "" {
				meta.ETag = entry.ETag
			}
			if entry.LastModified != "" && meta.LastModified == "" {
				meta.LastModified = entry.LastModified
			}
			if entry.ContentType != "" && meta.ContentType == "" {
				meta.ContentType = entry.ContentType
			}
			if entry.URL != "" && meta.URL == "" {
				meta.URL = entry.URL
			}
		}
	}
	return &meta, TierS3, true, nil
}

const s3ReadRequestTimeout = 30 * time.Second

type s3ReadSeeker struct {
	client    *s3.Client
	bucket    string
	key       string
	totalSize int64
	offset    int64
	ctx       context.Context
	cancel    context.CancelFunc

	mu        sync.Mutex
	buf       []byte
	bufOffset int64
}

func (r *s3ReadSeeker) Size() int64 {
	return r.totalSize
}

func (r *s3ReadSeeker) Tier() StorageTier {
	return TierS3
}

func (r *s3ReadSeeker) Close() error {
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = nil
	return nil
}

func (r *s3ReadSeeker) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = r.offset + offset
	case io.SeekEnd:
		newOffset = r.totalSize + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if newOffset < 0 {
		return 0, errors.New("negative offset")
	}
	r.offset = newOffset
	return r.offset, nil
}

func (r *s3ReadSeeker) contextErr() error {
	if r.ctx != nil {
		return r.ctx.Err()
	}
	return nil
}

func (r *s3ReadSeeker) fetchWindowLocked(off, toRead int64) error {
	baseCtx := r.ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if err := baseCtx.Err(); err != nil {
		return err
	}

	fetchSize := int64(defaultS3ReadAheadSize)
	if toRead > fetchSize {
		fetchSize = toRead
	}
	if off+fetchSize > r.totalSize {
		fetchSize = r.totalSize - off
	}

	reqCtx, cancel := context.WithTimeout(baseCtx, s3ReadRequestTimeout)
	defer cancel()

	rangeHeader := fmt.Sprintf("bytes=%d-%d", off, off+fetchSize-1)
	resp, err := r.client.GetObject(reqCtx, &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return recordS3Outcome(err)
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	if err != nil && err != io.EOF {
		return recordS3Outcome(err)
	}
	RecordS3Success()

	r.buf = buf
	r.bufOffset = off
	return nil
}

func (r *s3ReadSeeker) Read(p []byte) (int, error) {
	if err := r.contextErr(); err != nil {
		return 0, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.contextErr(); err != nil {
		return 0, err
	}
	if r.offset >= r.totalSize {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if r.offset+toRead > r.totalSize {
		toRead = r.totalSize - r.offset
	}

	// 1. Check if covered by read-ahead buffer
	bufEnd := r.bufOffset + int64(len(r.buf))
	if r.buf == nil || r.offset < r.bufOffset || r.offset >= bufEnd {
		if err := r.fetchWindowLocked(r.offset, toRead); err != nil {
			return 0, err
		}
	}

	relStart := r.offset - r.bufOffset
	avail := int64(len(r.buf)) - relStart
	n := toRead
	if n > avail {
		n = avail
	}
	copy(p[:n], r.buf[relStart:relStart+n])
	r.offset += n
	return int(n), nil
}

func (r *s3ReadSeeker) ReadAt(p []byte, off int64) (int, error) {
	if err := r.contextErr(); err != nil {
		return 0, err
	}
	if off >= r.totalSize {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if off+toRead > r.totalSize {
		toRead = r.totalSize - off
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.contextErr(); err != nil {
		return 0, err
	}

	bufEnd := r.bufOffset + int64(len(r.buf))
	if r.buf == nil || off < r.bufOffset || off >= bufEnd {
		if err := r.fetchWindowLocked(off, toRead); err != nil {
			return 0, err
		}
	}

	relStart := off - r.bufOffset
	avail := int64(len(r.buf)) - relStart
	n := toRead
	if n > avail {
		n = avail
	}
	copy(p[:n], r.buf[relStart:relStart+n])
	return int(n), nil
}

func (b *S3StorageBackend) Open(ctx context.Context, key string) (StorageReadSeeker, error) {
	meta, exists, err := b.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	return b.openKnownSize(ctx, key, meta.TotalSize), nil
}

// OpenWithMeta opens an s3ReadSeeker without issuing a HeadObject call when meta.TotalSize is already known.
func (b *S3StorageBackend) OpenWithMeta(ctx context.Context, key string, meta *FileMeta, _ StorageTier) (StorageReadSeeker, error) {
	if meta != nil && meta.TotalSize > 0 {
		return b.openKnownSize(ctx, key, meta.TotalSize), nil
	}
	return b.Open(ctx, key)
}

func (b *S3StorageBackend) openKnownSize(ctx context.Context, key string, totalSize int64) StorageReadSeeker {
	baseCtx := ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	readerCtx, cancel := context.WithCancel(baseCtx)

	return &s3ReadSeeker{
		client:    b.client,
		bucket:    b.bucket,
		key:       b.monolithicKey(key),
		totalSize: totalSize,
		ctx:       readerCtx,
		cancel:    cancel,
	}
}

func (b *S3StorageBackend) OpenRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	targetKey := b.monolithicKey(key)
	var rangeHeader string
	if length > 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	} else {
		rangeHeader = fmt.Sprintf("bytes=%d-", offset)
	}

	resp, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(targetKey),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return nil, recordS3Outcome(err)
	}
	RecordS3Success()
	return resp.Body, nil
}

func (b *S3StorageBackend) HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error) {
	chunkKey := b.chunkKey(key, chunkIndex)
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(chunkKey),
	})
	if err != nil {
		if isS3NotFoundError(err) {
			RecordS3Success()
			return false, nil
		}
		RecordS3Failure(err)
		return false, err
	}
	RecordS3Success()
	return true, nil
}

func (b *S3StorageBackend) OpenChunkRange(ctx context.Context, key string, chunkIndex int, offset, length int64) (io.ReadCloser, error) {
	chunkKey := b.chunkKey(key, chunkIndex)
	var rangeHeader string
	if length > 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	} else if offset > 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-", offset)
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(chunkKey),
	}
	if rangeHeader != "" {
		input.Range = aws.String(rangeHeader)
	}

	resp, err := b.client.GetObject(ctx, input)
	if err != nil {
		return nil, recordS3Outcome(err)
	}
	RecordS3Success()
	return resp.Body, nil
}

func (b *S3StorageBackend) UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error {
	chunkKey := b.chunkKey(key, chunkIndex)
	var body io.Reader = r
	if size == 0 {
		body = bytes.NewReader(nil)
	} else if size > 0 {
		if rs, ok := r.(io.ReadSeeker); ok {
			pos, seekErr := rs.Seek(0, io.SeekCurrent)
			if seekErr == nil {
				if endPos, endErr := rs.Seek(0, io.SeekEnd); endErr == nil {
					if _, err := rs.Seek(pos, io.SeekStart); err != nil {
						return err
					}
					avail := endPos - pos
					if avail < size {
						return io.ErrUnexpectedEOF
					}
					if avail == size {
						body = rs
					} else if ra, okAt := r.(io.ReaderAt); okAt {
						body = io.NewSectionReader(ra, pos, size)
					} else {
						body = io.LimitReader(rs, size)
					}
				} else {
					body = rs
				}
			} else {
				body = rs
			}
		} else {
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return err
			}
			body = bytes.NewReader(buf)
		}
	}

	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(chunkKey),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	return recordS3Outcome(err)
}

func (b *S3StorageBackend) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	return b.RecomposeWithProgress(ctx, key, totalParts, meta, commitProgressFromContext(ctx))
}

func (b *S3StorageBackend) RecomposeWithProgress(ctx context.Context, key string, totalParts int, meta FileMeta, onProgress CommitProgressFunc) error {
	if totalParts <= 0 {
		return errors.New("cannot recompose with 0 parts")
	}
	return recordS3Outcome(b.recomposeWithProgressInner(ctx, key, totalParts, meta, onProgress))
}

func (b *S3StorageBackend) recomposeWithProgressInner(ctx context.Context, key string, totalParts int, meta FileMeta, onProgress CommitProgressFunc) error {

	report := func(stage string, done, total int) {
		if onProgress != nil {
			onProgress(stage, done, total)
		}
	}

	targetKey := b.monolithicKey(key)
	metaMap := b.metaToMap(meta)

	// 1. CreateMultipartUpload
	createResp, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(targetKey),
		Metadata: metaMap,
	})
	if err != nil {
		return err
	}

	uploadID := createResp.UploadId
	var completedSuccessfully bool
	defer func() {
		if !completedSuccessfully && uploadID != nil && *uploadID != "" {
			// Detach abort cleanup from a canceled/timed-out request context so multipart state never leaks in S3.
			abortCtx, abortCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer abortCancel()
			_, _ = b.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.bucket),
				Key:      aws.String(targetKey),
				UploadId: uploadID,
			})
		}
	}()

	completedParts := make([]types.CompletedPart, totalParts)
	chunkKeys := make([]types.ObjectIdentifier, totalParts)
	for i := 0; i < totalParts; i++ {
		chunkKeys[i] = types.ObjectIdentifier{Key: aws.String(b.chunkKey(key, i))}
	}

	report("recomposing", 0, totalParts)

	workers := clampConcurrency(b.recomposeConcurrency, 6)
	if workers > totalParts {
		workers = totalParts
	}

	recomposeCtx, cancelRecompose := context.WithCancel(ctx)
	defer cancelRecompose()

	jobs := make(chan int, totalParts)
	for i := 0; i < totalParts; i++ {
		jobs <- i
	}
	close(jobs)

	var progressMu sync.Mutex
	completedCount := 0
	var errOnce sync.Once
	var firstErr error

	// 2. Parallel UploadPartCopy with jittered retry; each slot writes exclusively to completedParts[i]
	// so CompleteMultipartUpload always receives parts in strict 1..totalParts order.
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs {
				if recomposeCtx.Err() != nil {
					return
				}

				partNumber := int32(i + 1)
				copySource := b.bucket + "/" + aws.ToString(chunkKeys[i].Key)

				var copyResp *s3.UploadPartCopyOutput
				var partErr error
				for attempt := 1; attempt <= 3; attempt++ {
					if recomposeCtx.Err() != nil {
						partErr = recomposeCtx.Err()
						break
					}

					copyResp, partErr = b.client.UploadPartCopy(recomposeCtx, &s3.UploadPartCopyInput{
						Bucket:     aws.String(b.bucket),
						Key:        aws.String(targetKey),
						UploadId:   uploadID,
						PartNumber: aws.Int32(partNumber),
						CopySource: aws.String(copySource),
					})
					if partErr == nil {
						if copyResp == nil || copyResp.CopyPartResult == nil || copyResp.CopyPartResult.ETag == nil {
							partErr = fmt.Errorf("UploadPartCopy returned empty ETag for part %d", partNumber)
						} else {
							break
						}
					}

					if attempt < 3 {
						jitter := time.Duration((time.Now().UnixNano()+int64(i*37+workerID*13))%50) * time.Millisecond
						backoff := time.Duration(75*attempt)*time.Millisecond + jitter
						timer := time.NewTimer(backoff)
						select {
						case <-recomposeCtx.Done():
							timer.Stop()
							partErr = recomposeCtx.Err()
							attempt = 3
						case <-timer.C:
						}
					}
				}

				if partErr != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("UploadPartCopy failed for part %d: %w", partNumber, partErr)
						cancelRecompose()
					})
					return
				}

				completedParts[i] = types.CompletedPart{
					PartNumber: aws.Int32(partNumber),
					ETag:       copyResp.CopyPartResult.ETag,
				}
				progressMu.Lock()
				completedCount++
				done := completedCount
				report("recomposing", done, totalParts)
				progressMu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	const maxDeleteBatch = 1000
	deleteBatches := (len(chunkKeys) + maxDeleteBatch - 1) / maxDeleteBatch
	finalizeTotal := 1 + deleteBatches
	report("finalizing", 0, finalizeTotal)

	// 3. CompleteMultipartUpload
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(targetKey),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		return fmt.Errorf("CompleteMultipartUpload failed: %w", err)
	}

	completedSuccessfully = true
	b.clearCachedMeta(key)
	finalizeStep := 1
	report("finalizing", finalizeStep, finalizeTotal)

	// 4. Clean up temporary chunk objects (batched to stay within S3's 1000-key limit)
	for start := 0; start < len(chunkKeys); start += maxDeleteBatch {
		end := start + maxDeleteBatch
		if end > len(chunkKeys) {
			end = len(chunkKeys)
		}
		_, _ = b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{
				Objects: chunkKeys[start:end],
				Quiet:   aws.Bool(true),
			},
		})
		finalizeStep++
		report("finalizing", finalizeStep, finalizeTotal)
	}

	return nil
}

// CommitLocalFile streams localPartPath into vod/<key> (using concurrent multipart UploadPart for > chunkSize files).
func (b *S3StorageBackend) CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error {
	return b.UploadFromFile(ctx, key, localPartPath, meta)
}

// UploadFromFile uploads an intact local file directly to S3 vod/<key> in a single pass
// using concurrent multipart UploadPart when larger than S3_CHUNK_SIZE (safe for > 5 GB objects).
func (b *S3StorageBackend) UploadFromFile(ctx context.Context, key string, filePath string, meta FileMeta) error {
	return b.UploadFromFileWithProgress(ctx, key, filePath, meta, commitProgressFromContext(ctx))
}

// UploadFromFileWithProgress streams filePath directly to vod/<key> while reporting CommitProgress updates.
func (b *S3StorageBackend) UploadFromFileWithProgress(ctx context.Context, key string, filePath string, meta FileMeta, onProgress CommitProgressFunc) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	return recordS3Outcome(b.uploadFileWithProgressInner(ctx, key, f, stat.Size(), meta, onProgress))
}

func (b *S3StorageBackend) uploadFileWithProgressInner(ctx context.Context, key string, f *os.File, fileSize int64, meta FileMeta, onProgress CommitProgressFunc) error {
	report := func(stage string, done, total int) {
		if onProgress != nil {
			onProgress(stage, done, total)
		}
	}

	targetKey := b.monolithicKey(key)
	meta.Completed = true
	meta.TotalSize = fileSize
	if meta.LastValidated == 0 {
		meta.LastValidated = time.Now().Unix()
	}

	partSize, isMultipart := CalculateDynamicChunkConfig(fileSize, b.chunkSize)
	totalParts := 1
	if isMultipart && partSize > 0 {
		totalParts = int((fileSize + partSize - 1) / partSize)
	}
	if !isMultipart || fileSize == 0 || partSize <= 0 || totalParts <= 1 {
		report("uploading", 0, 1)
		_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(b.bucket),
			Key:           aws.String(targetKey),
			Body:          f,
			ContentLength: aws.Int64(fileSize),
			Metadata:      b.metaToMap(meta),
		})
		FadviseDontNeed(f.Fd(), 0, 0)
		if err != nil {
			return err
		}
		b.clearCachedMeta(key)
		report("finalizing", 1, 1)
		return nil
	}

	createResp, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(targetKey),
		Metadata: b.metaToMap(meta),
	})
	if err != nil {
		return err
	}

	uploadID := createResp.UploadId
	var completedSuccessfully bool
	defer func() {
		if !completedSuccessfully && uploadID != nil && *uploadID != "" {
			abortCtx, abortCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer abortCancel()
			_, _ = b.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.bucket),
				Key:      aws.String(targetKey),
				UploadId: uploadID,
			})
		}
	}()

	report("uploading", 0, totalParts)

	completedParts := make([]types.CompletedPart, totalParts)
	workers := clampConcurrency(b.uploadConcurrency, 4)
	if workers > totalParts {
		workers = totalParts
	}

	uploadCtx, cancelUpload := context.WithCancel(ctx)
	defer cancelUpload()

	jobs := make(chan int, totalParts)
	for i := 0; i < totalParts; i++ {
		jobs <- i
	}
	close(jobs)

	var progressMu sync.Mutex
	completedCount := 0
	var errOnce sync.Once
	var firstErr error

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range jobs {
				if uploadCtx.Err() != nil {
					return
				}

				partOffset := int64(i) * partSize
				partLen := partSize
				if partOffset+partLen > fileSize {
					partLen = fileSize - partOffset
				}
				partNumber := int32(i + 1)

				var upResp *s3.UploadPartOutput
				var partErr error
				for attempt := 1; attempt <= 3; attempt++ {
					if uploadCtx.Err() != nil {
						partErr = uploadCtx.Err()
						break
					}

					sr := io.NewSectionReader(f, partOffset, partLen)
					upResp, partErr = b.client.UploadPart(uploadCtx, &s3.UploadPartInput{
						Bucket:        aws.String(b.bucket),
						Key:           aws.String(targetKey),
						UploadId:      uploadID,
						PartNumber:    aws.Int32(partNumber),
						Body:          sr,
						ContentLength: aws.Int64(partLen),
					})
					if partErr == nil {
						if upResp == nil || upResp.ETag == nil {
							partErr = fmt.Errorf("UploadPart returned empty ETag for part %d", partNumber)
						} else {
							break
						}
					}

					if attempt < 3 {
						jitter := time.Duration((time.Now().UnixNano()+int64(i*37+workerID*13))%50) * time.Millisecond
						backoff := time.Duration(75*attempt)*time.Millisecond + jitter
						timer := time.NewTimer(backoff)
						select {
						case <-uploadCtx.Done():
							timer.Stop()
							partErr = uploadCtx.Err()
							attempt = 3
						case <-timer.C:
						}
					}
				}

				if partErr != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("UploadPart failed for part %d: %w", partNumber, partErr)
						cancelUpload()
					})
					return
				}

				completedParts[i] = types.CompletedPart{
					PartNumber: aws.Int32(partNumber),
					ETag:       upResp.ETag,
				}
				FadviseDontNeed(f.Fd(), partOffset, partLen)

				progressMu.Lock()
				completedCount++
				done := completedCount
				report("uploading", done, totalParts)
				progressMu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	report("finalizing", 0, 1)
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(b.bucket),
		Key:      aws.String(targetKey),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		return fmt.Errorf("CompleteMultipartUpload failed: %w", err)
	}

	completedSuccessfully = true
	b.clearCachedMeta(key)
	FadviseDontNeed(f.Fd(), 0, 0)
	report("finalizing", 1, 1)
	return nil
}

func (b *S3StorageBackend) Delete(ctx context.Context, key string) error {
	targetKey := b.monolithicKey(key)
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(targetKey),
	})
	_ = b.DeleteChunks(ctx, key)
	b.clearCachedMeta(key)
	return err
}

func (b *S3StorageBackend) clearCachedMeta(key string) {
	b.mu.Lock()
	if b.metaCache != nil {
		delete(b.metaCache, key)
	}
	b.mu.Unlock()
}

func (b *S3StorageBackend) deleteKeyBatch(ctx context.Context, keys []types.ObjectIdentifier) error {
	const maxDeleteBatch = 1000
	var firstErr error
	for start := 0; start < len(keys); start += maxDeleteBatch {
		end := start + maxDeleteBatch
		if end > len(keys) {
			end = len(keys)
		}
		if _, err := b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{
				Objects: keys[start:end],
				Quiet:   aws.Bool(true),
			},
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// DeleteChunks deletes all uploaded temporary staging chunks for a single key under chunks/<key>/.
func (b *S3StorageBackend) DeleteChunks(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	if !IsS3Healthy() {
		return ErrStorageCircuitOpen
	}
	prefix := fmt.Sprintf("chunks/%s/", key)
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	var toDelete []types.ObjectIdentifier
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			if obj.Key != nil {
				toDelete = append(toDelete, types.ObjectIdentifier{Key: obj.Key})
			}
		}
	}
	if len(toDelete) == 0 {
		return nil
	}
	return b.deleteKeyBatch(ctx, toDelete)
}

// SweepStaleChunks removes orphaned staging chunk objects older than maxAge (default 24h).
func (b *S3StorageBackend) SweepStaleChunks(ctx context.Context, maxAge time.Duration) (int, error) {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	if !IsS3Healthy() {
		return 0, ErrStorageCircuitOpen
	}

	const prefix = "chunks/"
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	now := time.Now()
	var toDelete []types.ObjectIdentifier
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				continue
			}
			fullKey := *obj.Key
			rel := strings.TrimPrefix(fullKey, prefix)
			if idx := strings.LastIndex(rel, "/chunk_"); idx > 0 {
				assetKey := rel[:idx]
				if GlobalRegistry != nil && GlobalRegistry.IsKeyInUse(assetKey) {
					continue
				}
			}
			if now.Sub(aws.ToTime(obj.LastModified)) >= maxAge {
				toDelete = append(toDelete, types.ObjectIdentifier{Key: aws.String(fullKey)})
			}
		}
	}
	if len(toDelete) == 0 {
		return 0, nil
	}
	if err := b.deleteKeyBatch(ctx, toDelete); err != nil {
		return 0, err
	}
	return len(toDelete), nil
}

// UpdateMetadata records updated S3 metadata locally in memory and host-local CacheIndex,
// avoiding slow/5GB-capped S3 CopyObject self-copies.
func (b *S3StorageBackend) UpdateMetadata(ctx context.Context, key string, meta FileMeta) error {
	b.mu.Lock()
	if b.metaCache == nil {
		b.metaCache = make(map[string]FileMeta)
	}
	b.metaCache[key] = meta
	b.mu.Unlock()

	if idx := getGlobalCacheIndex(); idx != nil {
		idx.RecordValidation(key, meta, IndexTierS3)
	}
	return nil
}

func (b *S3StorageBackend) List(ctx context.Context) ([]StorageObjectInfo, error) {
	const prefix = "vod/"
	paginator := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})

	var result []StorageObjectInfo
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, obj := range page.Contents {
			fullKey := aws.ToString(obj.Key)
			key := strings.TrimPrefix(fullKey, prefix)
			if key == "" {
				continue
			}
			result = append(result, StorageObjectInfo{
				Key:          key,
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}
	return result, nil
}
