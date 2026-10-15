package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func runStorageContractTest(t *testing.T, backend StorageBackend, prepareLocalFile func(key string, data []byte) string) {
	ctx := context.Background()
	key := "contract_test.mp4"
	testData := make([]byte, 256*1024) // 256KB
	_, _ = rand.Read(testData)

	// 1. Initial Stat should return not found
	meta, exists, err := backend.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat error on nonexistent key: %v", err)
	}
	if exists {
		t.Fatalf("Expected key not to exist initially")
	}

	// 2. Commit a file
	localPart := prepareLocalFile(key, testData)
	initMeta := FileMeta{
		URL:           "http://example.com/video.mp4",
		TotalSize:     int64(len(testData)),
		ContentType:   "video/mp4",
		ETag:          "etag-12345",
		LastModified:  "Mon, 01 Jan 2026 00:00:00 GMT",
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}

	if err := backend.CommitLocalFile(ctx, key, localPart, initMeta); err != nil {
		t.Fatalf("CommitLocalFile error: %v", err)
	}

	// 3. Stat should now return exists
	meta, exists, err = backend.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat error on committed key: %v", err)
	}
	if !exists {
		t.Fatalf("Expected key to exist after commit")
	}
	if meta.TotalSize != int64(len(testData)) {
		t.Errorf("Expected TotalSize %d, got %d", len(testData), meta.TotalSize)
	}
	if meta.ETag != "etag-12345" {
		t.Errorf("Expected ETag 'etag-12345', got %q", meta.ETag)
	}

	// 4. Open and verify StorageReadSeeker
	r, err := backend.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	defer r.Close()

	if r.Size() != int64(len(testData)) {
		t.Errorf("Expected Size() %d, got %d", len(testData), r.Size())
	}

	// Sequential Read
	readBuf := make([]byte, len(testData))
	n, err := io.ReadFull(r, readBuf)
	if err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	if n != len(testData) {
		t.Fatalf("Expected %d bytes read, got %d", len(testData), n)
	}
	if !bytes.Equal(readBuf, testData) {
		t.Errorf("Read data does not match test data")
	}

	// Seek to middle and Read
	midOffset := int64(len(testData) / 2)
	pos, err := r.Seek(midOffset, io.SeekStart)
	if err != nil || pos != midOffset {
		t.Fatalf("Seek failed: pos=%d, err=%v", pos, err)
	}
	halfBuf := make([]byte, 1024)
	n, err = io.ReadFull(r, halfBuf)
	if err != nil {
		t.Fatalf("Read after seek error: %v", err)
	}
	if !bytes.Equal(halfBuf, testData[midOffset:midOffset+1024]) {
		t.Errorf("Data at seek offset does not match")
	}

	// ReadAt random access
	atBuf := make([]byte, 2048)
	n, err = r.ReadAt(atBuf, 4096)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt error: %v", err)
	}
	if !bytes.Equal(atBuf[:n], testData[4096:4096+2048]) {
		t.Errorf("ReadAt content mismatch")
	}

	// 5. OpenRange test
	rangeReader, err := backend.OpenRange(ctx, key, 1000, 500)
	if err != nil {
		t.Fatalf("OpenRange error: %v", err)
	}
	defer rangeReader.Close()
	rangeData, err := io.ReadAll(rangeReader)
	if err != nil {
		t.Fatalf("ReadAll on OpenRange error: %v", err)
	}
	if len(rangeData) != 500 {
		t.Fatalf("Expected 500 bytes from OpenRange, got %d", len(rangeData))
	}
	if !bytes.Equal(rangeData, testData[1000:1500]) {
		t.Errorf("OpenRange slice data mismatch")
	}

	// 6. UpdateMetadata
	newMeta := *meta
	newMeta.ETag = "etag-updated-67890"
	newMeta.LastValidated = time.Now().Unix() + 3600
	if err := backend.UpdateMetadata(ctx, key, newMeta); err != nil {
		t.Fatalf("UpdateMetadata error: %v", err)
	}
	metaAfterUpdate, _, err := backend.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat after update error: %v", err)
	}
	if metaAfterUpdate.ETag != "etag-updated-67890" {
		t.Errorf("Expected updated ETag 'etag-updated-67890', got %q", metaAfterUpdate.ETag)
	}

	// 7. Delete
	if err := backend.Delete(ctx, key); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	_, existsAfterDelete, err := backend.Stat(ctx, key)
	if err != nil {
		t.Fatalf("Stat after delete error: %v", err)
	}
	if existsAfterDelete {
		t.Errorf("Expected key to not exist after delete")
	}
}

func TestLocalStorageBackend_Contract(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "sc2_local_storage_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	cacheDir := filepath.Join(tempDir, "files")
	tmpDir := filepath.Join(tempDir, "tmp")
	backend := NewLocalStorageBackend(cacheDir, tmpDir)

	runStorageContractTest(t, backend, func(key string, data []byte) string {
		partPath := filepath.Join(tmpDir, key+".part")
		if err := os.WriteFile(partPath, data, 0644); err != nil {
			t.Fatalf("Failed to write test part file: %v", err)
		}
		return partPath
	})
}

func TestS3StorageBackend_Contract(t *testing.T) {
	mockS3 := NewMockS3Server("test-bucket")
	defer mockS3.Close()

	tempDir, err := os.MkdirTemp("", "sc2_s3_storage_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "test-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    64 * 1024,
	})
	if err != nil {
		t.Fatalf("Failed to init S3StorageBackend: %v", err)
	}

	runStorageContractTest(t, backend, func(key string, data []byte) string {
		partPath := filepath.Join(tempDir, key+".part")
		if err := os.WriteFile(partPath, data, 0644); err != nil {
			t.Fatalf("Failed to write test part file: %v", err)
		}
		return partPath
	})
}

func TestS3StorageBackend_ChunkingAndRecomposition(t *testing.T) {
	mockS3 := NewMockS3Server("recompose-bucket")
	defer mockS3.Close()

	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "recompose-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    32 * 1024, // 32KB chunks for test
	})
	if err != nil {
		t.Fatalf("Failed to init S3StorageBackend: %v", err)
	}

	ctx := context.Background()
	key := "4k_movie_test.mkv"
	chunkSize := 32 * 1024
	totalChunks := 4
	fullData := make([]byte, totalChunks*chunkSize)
	_, _ = rand.Read(fullData)

	// 1. Upload chunks one by one
	for i := 0; i < totalChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		chunkSlice := fullData[start:end]

		// Verify HasChunk is false before upload
		has, err := backend.HasChunk(ctx, key, i)
		if err != nil || has {
			t.Fatalf("HasChunk expected false before upload for chunk %d", i)
		}

		err = backend.UploadChunk(ctx, key, i, bytes.NewReader(chunkSlice), int64(len(chunkSlice)))
		if err != nil {
			t.Fatalf("UploadChunk %d error: %v", i, err)
		}

		// Verify HasChunk is true after upload
		has, err = backend.HasChunk(ctx, key, i)
		if err != nil || !has {
			t.Fatalf("HasChunk expected true after upload for chunk %d", i)
		}

		// Read byte range inside chunk
		chunkReader, err := backend.OpenChunkRange(ctx, key, i, 100, 50)
		if err != nil {
			t.Fatalf("OpenChunkRange error: %v", err)
		}
		subBytes, err := io.ReadAll(chunkReader)
		chunkReader.Close()
		if err != nil {
			t.Fatalf("ReadAll chunk error: %v", err)
		}
		if !bytes.Equal(subBytes, chunkSlice[100:150]) {
			t.Errorf("Chunk sub-slice mismatch for chunk %d", i)
		}
	}

	// 2. Recompose chunks into monolithic object
	meta := FileMeta{
		URL:           "http://example.com/4k_movie.mkv",
		TotalSize:     int64(len(fullData)),
		ContentType:   "video/x-matroska",
		ETag:          "movie-etag",
		LastModified:  "Mon, 01 Jan 2026 00:00:00 GMT",
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}

	err = backend.Recompose(ctx, key, totalChunks, meta)
	if err != nil {
		t.Fatalf("Recompose error: %v", err)
	}

	// 3. Verify monolithic file exists and matches fullData byte-for-byte
	statMeta, exists, err := backend.Stat(ctx, key)
	if err != nil || !exists {
		t.Fatalf("Expected monolithic object to exist after recompose: exists=%v, err=%v", exists, err)
	}
	if statMeta.TotalSize != int64(len(fullData)) {
		t.Errorf("Expected TotalSize %d, got %d", len(fullData), statMeta.TotalSize)
	}

	reader, err := backend.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open monolithic file error: %v", err)
	}
	defer reader.Close()

	recomposedBytes := make([]byte, len(fullData))
	if _, err := io.ReadFull(reader, recomposedBytes); err != nil {
		t.Fatalf("ReadFull recomposed file error: %v", err)
	}
	if !bytes.Equal(recomposedBytes, fullData) {
		t.Errorf("Recomposed file bytes do not match original bytes byte-for-byte!")
	}

	// 4. Verify temporary chunks were cleaned up
	for i := 0; i < totalChunks; i++ {
		has, err := backend.HasChunk(ctx, key, i)
		if err != nil {
			t.Fatalf("HasChunk after recompose error: %v", err)
		}
		if has {
			t.Errorf("Expected chunk %d to be deleted after recompose", i)
		}
	}
}

func TestS3MultiViewerDisparateOffsets(t *testing.T) {
	mockS3 := NewMockS3Server("multiviewer-bucket")
	defer mockS3.Close()

	chunkSize := int64(32 * 1024) // 32KB
	s3Config := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "multiviewer-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    chunkSize,
	}

	backend, err := NewS3StorageBackend(s3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	oldStorage := GlobalStorage
	GlobalStorage = backend
	defer func() { GlobalStorage = oldStorage }()

	InitRegistry()

	tempDir, err := os.MkdirTemp("", "sc2_multiviewer_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	key := "4k_avatar.mkv"
	totalSize := int64(128 * 1024) // 128KB (4 chunks of 32KB)
	fullData := make([]byte, totalSize)
	_, _ = rand.Read(fullData)

	partPath := filepath.Join(tempDir, key+".part")
	if err := os.WriteFile(partPath, fullData, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	metaPath := filepath.Join(tempDir, key+".json")

	state := NewFileCacheState(key, "http://example.com/avatar.mkv", filepath.Join(tempDir, key), partPath, metaPath, "video/x-matroska", "avatar-etag", "now", totalSize)
	GlobalRegistry.Files[key] = state

	// Pre-upload chunks 0 and 1 to S3 (representing chunks finished by Viewer 1)
	ctx := context.Background()
	_ = backend.UploadChunk(ctx, key, 0, bytes.NewReader(fullData[0:32768]), 32768)
	_ = backend.UploadChunk(ctx, key, 1, bytes.NewReader(fullData[32768:65536]), 32768)
	state.ChunkManager.uploaded[0] = true
	state.ChunkManager.uploaded[1] = true
	state.RangeSet.Add(0, 65536)

	// Viewer 1 is active at 80% (byte 65536, chunk 2, currently in local scratch, not yet in S3)
	state.RangeSet.Add(65536, 98304)

	// Now Viewer 2 joins and starts watching from byte 0!
	reader2 := NewCacheReader(ctx, state, nil)
	defer reader2.Close()

	readBuf2 := make([]byte, 1024)
	n2, err := reader2.ReadAt(readBuf2, 0)
	if err != nil {
		t.Fatalf("Viewer 2 ReadAt error: %v", err)
	}
	if n2 != 1024 {
		t.Fatalf("Viewer 2 expected 1024 bytes, got %d", n2)
	}
	if !bytes.Equal(readBuf2, fullData[0:1024]) {
		t.Errorf("Viewer 2 bytes mismatch! Data served from S3 chunk does not match original bytes.")
	}

	// Viewer 2 seeks to chunk 1 (offset 40000)
	seekBuf2 := make([]byte, 2048)
	n2, err = reader2.ReadAt(seekBuf2, 40000)
	if err != nil {
		t.Fatalf("Viewer 2 seek ReadAt error: %v", err)
	}
	if !bytes.Equal(seekBuf2[:n2], fullData[40000:40000+2048]) {
		t.Errorf("Viewer 2 seek bytes mismatch from chunk 1 in S3!")
	}

	// Viewer 1 reads at chunk 2 (offset 70000) - served from local scratch
	reader1 := NewCacheReader(ctx, state, nil)
	defer reader1.Close()

	readBuf1 := make([]byte, 1024)
	n1, err := reader1.ReadAt(readBuf1, 70000)
	if err != nil {
		t.Fatalf("Viewer 1 ReadAt error: %v", err)
	}
	if !bytes.Equal(readBuf1[:n1], fullData[70000:71024]) {
		t.Errorf("Viewer 1 bytes mismatch from scratch file!")
	}
}

func TestS3RevalidationWithOrigin(t *testing.T) {
	mockS3 := NewMockS3Server("reval-bucket")
	defer mockS3.Close()

	s3Config := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "reval-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    32 * 1024,
	}

	backend, err := NewS3StorageBackend(s3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	oldStorage := GlobalStorage
	GlobalStorage = backend
	defer func() { GlobalStorage = oldStorage }()

	InitRegistry()

	key := "movie_reval.mp4"
	testData := []byte("hello streamcache s3 revalidation")

	// Pre-commit file in S3 with old LastValidated (expired)
	expiredTime := time.Now().Add(-2 * time.Hour).Unix()
	initMeta := FileMeta{
		URL:           "http://example.com/movie_reval.mp4",
		TotalSize:     int64(len(testData)),
		ContentType:   "video/mp4",
		ETag:          "etag-v1",
		LastModified:  "Wed, 01 Jan 2026 00:00:00 GMT",
		LastValidated: expiredTime,
		Completed:     true,
	}

	tempDir, err := os.MkdirTemp("", "sc2_reval_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	partPath := filepath.Join(tempDir, key+".part")
	if err := os.WriteFile(partPath, testData, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	if err := backend.CommitLocalFile(context.Background(), key, partPath, initMeta); err != nil {
		t.Fatalf("CommitLocalFile error: %v", err)
	}

	// 1. Origin returns 304 Not Modified
	originServer304 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "etag-v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", "etag-v1")
		w.Header().Set("Content-Length", strconv.Itoa(len(testData)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testData)
	}))
	defer originServer304.Close()

	valid, err := Revalidate(key, originServer304.URL, nil)
	if err != nil {
		t.Fatalf("Revalidate error: %v", err)
	}
	if !valid {
		t.Fatalf("Expected cache to be valid on 304")
	}

	// Verify LastValidated was updated in S3 metadata
	updatedMeta, exists, err := backend.Stat(context.Background(), key)
	if err != nil || !exists {
		t.Fatalf("Expected S3 object to exist: %v", err)
	}
	if updatedMeta.LastValidated <= expiredTime {
		t.Errorf("Expected LastValidated to be bumped, was %d, now %d", expiredTime, updatedMeta.LastValidated)
	}

	// 2. Origin returns 200 with new ETag -> should evict S3 object
	originServerModified := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "etag-v2-modified")
		w.Header().Set("Content-Length", strconv.Itoa(len(testData)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testData)
	}))
	defer originServerModified.Close()

	// Expire the cache again
	updatedMeta.LastValidated = time.Now().Add(-2 * time.Hour).Unix()
	_ = backend.UpdateMetadata(context.Background(), key, *updatedMeta)

	valid, err = Revalidate(key, originServerModified.URL, nil)
	if err != nil {
		t.Fatalf("Revalidate modified error: %v", err)
	}
	if valid {
		t.Errorf("Expected cache to be invalid when origin content changed")
	}

	// Verify S3 object was evicted
	_, existsAfterEviction, _ := backend.Stat(context.Background(), key)
	if existsAfterEviction {
		t.Errorf("Expected S3 object to be deleted upon revalidation eviction")
	}
}

