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
	client          *s3.Client
	bucket          string
	chunkSize       int64
	scratchMaxBytes int64
	mu              sync.RWMutex
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

	scratchMax := cfg.ScratchMaxBytes
	if scratchMax <= 0 {
		scratchMax = 4 * 1024 * 1024 * 1024
	}

	return &S3StorageBackend{
		client:          client,
		bucket:          cfg.Bucket,
		chunkSize:       chunkSize,
		scratchMaxBytes: scratchMax,
	}, nil
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
	targetKey := b.monolithicKey(key)
	resp, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(targetKey),
	})
	if err != nil {
		var nfe *types.NoSuchKey
		var notFound *types.NotFound
		if errors.As(err, &nfe) || errors.As(err, &notFound) || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound") {
			return nil, false, nil
		}
		return nil, false, err
	}

	meta := b.mapToMeta(resp.Metadata)
	if meta.TotalSize == 0 && resp.ContentLength != nil {
		meta.TotalSize = *resp.ContentLength
	}
	if meta.ETag == "" && resp.ETag != nil {
		meta.ETag = strings.Trim(*resp.ETag, "\"")
	}
	meta.Completed = true
	return &meta, true, nil
}

type s3ReadSeeker struct {
	client    *s3.Client
	bucket    string
	key       string
	totalSize int64
	offset    int64

	mu        sync.Mutex
	buf       []byte
	bufOffset int64
}

func (r *s3ReadSeeker) Size() int64 {
	return r.totalSize
}

func (r *s3ReadSeeker) Close() error {
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

func (r *s3ReadSeeker) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.offset >= r.totalSize {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if r.offset+toRead > r.totalSize {
		toRead = r.totalSize - r.offset
	}

	// 1. Check if covered by read-ahead buffer
	bufEnd := r.bufOffset + int64(len(r.buf))
	if r.buf != nil && r.offset >= r.bufOffset && r.offset < bufEnd {
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

	// 2. Fetch new window into buffer with read-ahead
	fetchSize := int64(defaultS3ReadAheadSize)
	if toRead > fetchSize {
		fetchSize = toRead
	}
	if r.offset+fetchSize > r.totalSize {
		fetchSize = r.totalSize - r.offset
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", r.offset, r.offset+fetchSize-1)
	resp, err := r.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	if err != nil && err != io.EOF {
		return 0, err
	}

	r.buf = buf
	r.bufOffset = r.offset

	n := int64(len(p))
	if n > int64(len(r.buf)) {
		n = int64(len(r.buf))
	}
	copy(p[:n], r.buf[:n])
	r.offset += n
	return int(n), nil
}

func (r *s3ReadSeeker) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.totalSize {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if off+toRead > r.totalSize {
		toRead = r.totalSize - off
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	bufEnd := r.bufOffset + int64(len(r.buf))
	if r.buf != nil && off >= r.bufOffset && off < bufEnd {
		relStart := off - r.bufOffset
		avail := int64(len(r.buf)) - relStart
		n := toRead
		if n > avail {
			n = avail
		}
		copy(p[:n], r.buf[relStart:relStart+n])
		return int(n), nil
	}

	fetchSize := int64(defaultS3ReadAheadSize)
	if toRead > fetchSize {
		fetchSize = toRead
	}
	if off+fetchSize > r.totalSize {
		fetchSize = r.totalSize - off
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", off, off+fetchSize-1)
	resp, err := r.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(r.key),
		Range:  aws.String(rangeHeader),
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	buf, err := io.ReadAll(resp.Body)
	if err != nil && err != io.EOF {
		return 0, err
	}

	r.buf = buf
	r.bufOffset = off

	n := toRead
	if n > int64(len(r.buf)) {
		n = int64(len(r.buf))
	}
	copy(p[:n], r.buf[:n])
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

	return &s3ReadSeeker{
		client:    b.client,
		bucket:    b.bucket,
		key:       b.monolithicKey(key),
		totalSize: meta.TotalSize,
	}, nil
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
		return nil, err
	}
	return resp.Body, nil
}

func (b *S3StorageBackend) HasChunk(ctx context.Context, key string, chunkIndex int) (bool, error) {
	chunkKey := b.chunkKey(key, chunkIndex)
	_, err := b.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(chunkKey),
	})
	if err != nil {
		var nfe *types.NoSuchKey
		var notFound *types.NotFound
		if errors.As(err, &nfe) || errors.As(err, &notFound) || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NotFound") {
			return false, nil
		}
		return false, err
	}
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
		return nil, err
	}
	return resp.Body, nil
}

func (b *S3StorageBackend) UploadChunk(ctx context.Context, key string, chunkIndex int, r io.Reader, size int64) error {
	chunkKey := b.chunkKey(key, chunkIndex)
	var body io.Reader = r
	if size > 0 {
		// Read into memory or limit reader
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}

	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(chunkKey),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	return err
}

func (b *S3StorageBackend) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	if totalParts <= 0 {
		return errors.New("cannot recompose with 0 parts")
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
	completedParts := make([]types.CompletedPart, 0, totalParts)
	chunkKeys := make([]types.ObjectIdentifier, 0, totalParts)

	// 2. UploadPartCopy for each chunk
	for i := 0; i < totalParts; i++ {
		partNumber := int32(i + 1)
		chunkKey := b.chunkKey(key, i)
		chunkKeys = append(chunkKeys, types.ObjectIdentifier{Key: aws.String(chunkKey)})

		copySource := b.bucket + "/" + chunkKey
		copyResp, err := b.client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket:     aws.String(b.bucket),
			Key:        aws.String(targetKey),
			UploadId:   uploadID,
			PartNumber: aws.Int32(partNumber),
			CopySource: aws.String(copySource),
		})
		if err != nil {
			_, _ = b.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
				Bucket:   aws.String(b.bucket),
				Key:      aws.String(targetKey),
				UploadId: uploadID,
			})
			return fmt.Errorf("UploadPartCopy failed for part %d: %w", partNumber, err)
		}

		completedParts = append(completedParts, types.CompletedPart{
			PartNumber: aws.Int32(partNumber),
			ETag:       copyResp.CopyPartResult.ETag,
		})
	}

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

	// 4. Clean up temporary chunk objects
	if len(chunkKeys) > 0 {
		_, _ = b.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(b.bucket),
			Delete: &types.Delete{
				Objects: chunkKeys,
				Quiet:   aws.Bool(true),
			},
		})
	}

	return nil
}

func (b *S3StorageBackend) CommitLocalFile(ctx context.Context, key string, localPartPath string, meta FileMeta) error {
	f, err := os.Open(localPartPath)
	if err != nil {
		return err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return err
	}

	targetKey := b.monolithicKey(key)
	meta.Completed = true
	meta.TotalSize = stat.Size()
	meta.LastValidated = time.Now().Unix()

	_, err = b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(targetKey),
		Body:          f,
		ContentLength: aws.Int64(stat.Size()),
		Metadata:      b.metaToMap(meta),
	})
	return err
}

func (b *S3StorageBackend) Delete(ctx context.Context, key string) error {
	targetKey := b.monolithicKey(key)
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(targetKey),
	})
	return err
}

func (b *S3StorageBackend) UpdateMetadata(ctx context.Context, key string, meta FileMeta) error {
	targetKey := b.monolithicKey(key)
	copySource := b.bucket + "/" + targetKey
	_, err := b.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(targetKey),
		CopySource:        aws.String(copySource),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata:          b.metaToMap(meta),
	})
	return err
}
