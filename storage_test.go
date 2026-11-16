package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
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
	tempDir, err := os.MkdirTemp("", "streamcache_local_storage_test_*")
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

	tempDir, err := os.MkdirTemp("", "streamcache_s3_storage_test_*")
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

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

	tempDir, err := os.MkdirTemp("", "streamcache_multiviewer_*")
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

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

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

	tempDir, err := os.MkdirTemp("", "streamcache_reval_*")
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

func TestS3ViewerTransitionAcrossRecomposition(t *testing.T) {
	mockS3 := NewMockS3Server("transition-bucket")
	defer mockS3.Close()

	chunkSize := int64(32 * 1024) // 32 KB chunks
	s3Config := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "transition-bucket",
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

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

	key := "4k_transition_movie.mkv"
	totalSize := int64(128 * 1024) // 128 KB -> 4 chunks
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 17) % 256)
	}

	tempDir, err := os.MkdirTemp("", "streamcache_transition_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	partPath := filepath.Join(tempDir, key+".part")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	_ = f.Truncate(totalSize)

	// Write first 2 chunks (64 KB) to scratch file
	_, _ = f.WriteAt(fullData[:64*1024], 0)
	_ = f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	state := &FileCacheState{
		Key:         key,
		Path:        filepath.Join(tempDir, key),
		PartPath:    partPath,
		MetaPath:    filepath.Join(tempDir, key+".meta"),
		TotalSize:   totalSize,
		RangeSet:    NewRangeSet(),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int64),
		NotifyChan:  make(chan struct{}, 10),
		ContentType: "video/x-matroska",
	}
	state.RangeSet.Add(0, 64*1024)

	chunkMgr := NewS3ChunkManager(state, backend, chunkSize)
	state.ChunkManager = chunkMgr

	// 1. Upload first 2 chunks
	chunkMgr.CheckAndTriggerUploads()
	chunkMgr.wg.Wait()

	if !chunkMgr.IsChunkUploaded(0) || !chunkMgr.IsChunkUploaded(1) {
		t.Fatalf("Expected chunks 0 and 1 to be uploaded to S3")
	}

	// 2. Viewer 3 arrives while stream is partially uploaded (trailing viewer)
	reader3 := NewCacheReader(ctx, state, nil)
	defer reader3.Close()

	// Viewer 3 reads 16 KB from chunk 0
	readBuf1 := make([]byte, 16*1024)
	n, err := reader3.ReadAt(readBuf1, 0)
	if err != nil {
		t.Fatalf("Viewer 3 initial ReadAt error: %v", err)
	}
	if n != len(readBuf1) || !bytes.Equal(readBuf1, fullData[:16*1024]) {
		t.Fatalf("Viewer 3 data mismatch while reading Chunk 0 from S3")
	}

	// Verify reader3 has chunkBuf populated
	reader3.chunkBufMu.Lock()
	if reader3.chunkBuf == nil || reader3.chunkBufIdx != 0 {
		reader3.chunkBufMu.Unlock()
		t.Fatalf("Expected reader3 to have chunkBuf populated for chunk 0")
	}
	reader3.chunkBufMu.Unlock()

	// 3. Coordinator downloads remaining 2 chunks (chunks 2 and 3)
	f2, err := os.OpenFile(partPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	_, _ = f2.WriteAt(fullData[64*1024:], 64*1024)
	_ = f2.Close()
	state.RangeSet.Add(64*1024, 128*1024)

	// Trigger uploads for remaining chunks and server-side recomposition
	chunkMgr.CheckAndTriggerUploads()
	chunkMgr.wg.Wait()

	// Wait for recomposition to seal
	deadline := time.Now().Add(5 * time.Second)
	for !chunkMgr.IsRecomposed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !chunkMgr.IsRecomposed() {
		t.Fatalf("Expected chunk manager to be recomposed")
	}

	// Simulate cache finalization: scratch file removed, chunk objects removed
	_ = os.Remove(partPath)

	// Verify temporary chunks in mock S3 are deleted
	chunk0Key := fmt.Sprintf("chunks/%s/chunk_%05d.bin", key, 0)
	if mockS3.HasObject("transition-bucket", chunk0Key) {
		t.Fatalf("Expected chunk 0 to be deleted from S3 after recomposition")
	}

	// Verify monolithic object exists in S3
	if !mockS3.HasObject("transition-bucket", "vod/"+key) {
		t.Fatalf("Expected monolithic object vod/%s to exist in S3", key)
	}

	// 4. Viewer 3 continues streaming across the transition:
	// A. Read remaining part of chunk 0 (offset 16384 to 32768)
	readBuf2 := make([]byte, 16*1024)
	n2, err := reader3.ReadAt(readBuf2, 16*1024)
	if err != nil {
		t.Fatalf("Viewer 3 ReadAt after recomposition error: %v", err)
	}
	if n2 != len(readBuf2) || !bytes.Equal(readBuf2, fullData[16*1024:32*1024]) {
		t.Fatalf("Viewer 3 data mismatch after recomposition transition")
	}

	// B. Read from chunk 3 (offset 96000, 32 KB)
	readBuf3 := make([]byte, 32*1024)
	n3, err := reader3.ReadAt(readBuf3, 96000)
	if err != nil {
		t.Fatalf("Viewer 3 ReadAt late offset error: %v", err)
	}
	if n3 != len(readBuf3) || !bytes.Equal(readBuf3, fullData[96000:96000+32*1024]) {
		t.Fatalf("Viewer 3 data mismatch at offset 96000")
	}

	// C. Verify reader3 transitioned to using s3Reader (s3ReadSeeker)
	reader3.s3ReaderMu.Lock()
	if reader3.s3Reader == nil {
		reader3.s3ReaderMu.Unlock()
		t.Fatalf("Expected reader3 to have transitioned to s3Reader!")
	}
	reader3.s3ReaderMu.Unlock()
}

func TestS3DynamicChunkSizer(t *testing.T) {
	// 1. Small file (2 MB) with default 64 MB config -> single PutObject bypass
	sz, isMulti := CalculateDynamicChunkConfig(2*1024*1024, 64*1024*1024)
	if isMulti {
		t.Errorf("Expected 2MB file to bypass multipart, got isMultipart=true")
	}
	if sz != 2*1024*1024 {
		t.Errorf("Expected chunk size 2MB, got %d", sz)
	}

	// 2. 60 GB movie with default 64 MB config -> ~938 parts
	sz, isMulti = CalculateDynamicChunkConfig(60*1024*1024*1024, 64*1024*1024)
	if !isMulti {
		t.Errorf("Expected 60GB file to use multipart")
	}
	if sz != 64*1024*1024 {
		t.Errorf("Expected chunk size 64MB, got %d", sz)
	}
	parts := (60*1024*1024*1024 + sz - 1) / sz
	if parts > 10000 {
		t.Errorf("Parts count %d exceeds S3 10,000 limit", parts)
	}

	// 3. 1 TB massive media file with default 64 MB config -> must scale up chunk size so parts <= 10000
	oneTB := int64(1024) * 1024 * 1024 * 1024
	sz, isMulti = CalculateDynamicChunkConfig(oneTB, 64*1024*1024)
	if !isMulti {
		t.Errorf("Expected 1TB file to use multipart")
	}
	parts = (oneTB + sz - 1) / sz
	if parts > 10000 {
		t.Errorf("Scaled parts count %d exceeds S3 10,000 limit for 1TB file (chunkSize: %d)", parts, sz)
	}
	if sz < 100*1024*1024 {
		t.Errorf("Expected chunk size to scale up to >=100MB for 1TB file, got %d", sz)
	}

	// 4. Explicit test chunk size (e.g. 32 KB) -> should honor test configuration
	sz, isMulti = CalculateDynamicChunkConfig(128*1024, 32*1024)
	if !isMulti {
		t.Errorf("Expected explicit small test chunk size to allow multipart testing")
	}
	if sz != 32*1024 {
		t.Errorf("Expected 32KB test chunk size, got %d", sz)
	}
}

func TestS3SmallFileSinglePutObject(t *testing.T) {
	mockS3 := NewMockS3Server("smallfile-bucket")
	defer mockS3.Close()

	s3Config := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "smallfile-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    64 * 1024 * 1024, // 64 MB default
	}

	backend, err := NewS3StorageBackend(s3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	oldStorage := GlobalStorage
	GlobalStorage = backend
	defer func() { GlobalStorage = oldStorage }()

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

	key := "small_sample.mp4"
	totalSize := int64(2 * 1024 * 1024) // 2 MB (< 5 MB)
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i % 256)
	}

	tempDir, err := os.MkdirTemp("", "streamcache_small_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	partPath := filepath.Join(tempDir, key+".part")
	if err := os.WriteFile(partPath, data, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	state := &FileCacheState{
		Key:         key,
		Path:        filepath.Join(tempDir, key),
		PartPath:    partPath,
		MetaPath:    filepath.Join(tempDir, key+".meta"),
		TotalSize:   totalSize,
		RangeSet:    NewRangeSet(),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int64),
		NotifyChan:  make(chan struct{}, 10),
		ContentType: "video/mp4",
	}

	chunkMgr := NewS3ChunkManager(state, backend, s3Config.ChunkSize)
	state.ChunkManager = chunkMgr

	if chunkMgr.IsMultipart() {
		t.Fatalf("Expected 2MB file to be non-multipart")
	}

	// Add entire 2MB to range set and trigger check
	state.RangeSet.Add(0, totalSize)
	chunkMgr.CheckAndTriggerUploads()
	chunkMgr.wg.Wait()

	// Wait for recompose/commit
	deadline := time.Now().Add(3 * time.Second)
	for !chunkMgr.IsRecomposed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !chunkMgr.IsRecomposed() {
		t.Fatalf("Expected small file to be committed to S3")
	}

	// Verify monolithic object exists in S3 (vod/small_sample.mp4)
	if !mockS3.HasObject("smallfile-bucket", "vod/"+key) {
		t.Fatalf("Expected vod/%s to exist in S3 via single PutObject", key)
	}

	// Verify no multipart uploads or temporary chunks leaked
	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected 0 active multipart uploads, got %d", mockS3.ActiveUploads())
	}
}

func TestS3HolePunchingAndBlockDeallocation(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "streamcache_punch_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	filePath := filepath.Join(tempDir, "sparse_test.bin")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	defer f.Close()

	// Write 1 MB of non-zero data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = 0xAA
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	_ = f.Sync()

	// Punch hole for the first 512 KB
	err = PunchHole(f.Fd(), 0, 512*1024)
	if err != nil {
		t.Fatalf("PunchHole error: %v", err)
	}

	// Read back punched range: hole must read as all zeros!
	verifyBuf := make([]byte, 512*1024)
	n, err := f.ReadAt(verifyBuf, 0)
	if err != nil || n != len(verifyBuf) {
		t.Fatalf("ReadAt punched range error: %v", err)
	}
	for i, b := range verifyBuf {
		if b != 0 {
			t.Fatalf("Expected zero byte in punched hole at offset %d, got %x", i, b)
		}
	}

	// Remaining 512 KB should still have original data
	verifyBuf2 := make([]byte, 512*1024)
	n2, err := f.ReadAt(verifyBuf2, 512*1024)
	if err != nil || n2 != len(verifyBuf2) {
		t.Fatalf("ReadAt unpunched range error: %v", err)
	}
	if !bytes.Equal(verifyBuf2, data[512*1024:]) {
		t.Fatalf("Unpunched range data corrupted!")
	}
}

func TestS3BackpressureScratchBudget(t *testing.T) {
	mockS3 := NewMockS3Server("backpressure-bucket")
	defer mockS3.Close()

	s3Config := S3Config{
		Endpoint:            mockS3.URL(),
		Bucket:              "backpressure-bucket",
		Region:              "us-east-1",
		AccessKey:           "test-access",
		SecretKey:           "test-secret",
		UsePathStyle:        true,
		DisableSSL:          true,
		ChunkSize:           32 * 1024,
		ScratchMaxBytes:     64 * 1024, // 64 KB max budget = 2 chunks
		ScratchMinFreeBytes: 1024,
	}

	backend, err := NewS3StorageBackend(s3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "streamcache_bp_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	state := &FileCacheState{
		Key:       "bp_test.mkv",
		PartPath:  filepath.Join(tempDir, "bp_test.mkv.part"),
		TotalSize: 128 * 1024,
		RangeSet:  NewRangeSet(),
		Clients:   make(map[string]chan struct{}),
	}

	chunkMgr := NewS3ChunkManager(state, backend, s3Config.ChunkSize)
	chunkMgr.maxInFlightBytes = 64 * 1024 // 64 KB cap
	chunkMgr.minFreeDiskBytes = 1024

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Initial budget should pass immediately
	if err := chunkMgr.WaitScratchBudget(ctx); err != nil {
		t.Fatalf("Initial budget wait failed: %v", err)
	}

	// 2. Add 64 KB in-flight (hitting the cap)
	chunkMgr.AddInFlightBytes(64 * 1024)

	// 3. Trying to wait now should block!
	blockedChan := make(chan struct{})
	go func() {
		// WaitScratchBudget should block until space is released
		_ = chunkMgr.WaitScratchBudget(ctx)
		close(blockedChan)
	}()

	// Verify it is blocked
	select {
	case <-blockedChan:
		t.Fatalf("WaitScratchBudget should have blocked when in-flight bytes hit ceiling!")
	case <-time.After(50 * time.Millisecond):
		// Expected to be blocked
	}

	// 4. Simulate S3 chunk upload completing and releasing 32 KB
	chunkMgr.SubInFlightBytes(32 * 1024)

	// 5. Worker should now unblock!
	select {
	case <-blockedChan:
		// Successfully unblocked!
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("WaitScratchBudget failed to unblock after SubInFlightBytes released space!")
	}
}

func TestS3RecompositionAbortOnFailure(t *testing.T) {
	mockS3 := NewMockS3Server("abort-bucket")
	defer mockS3.Close()

	s3Config := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "abort-bucket",
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

	ctx := context.Background()
	key := "abort_test.mp4"
	chunkData := make([]byte, 32*1024)

	// Upload 2 chunks
	_ = backend.UploadChunk(ctx, key, 0, bytes.NewReader(chunkData), int64(len(chunkData)))
	_ = backend.UploadChunk(ctx, key, 1, bytes.NewReader(chunkData), int64(len(chunkData)))

	// Inject error on part 2 UploadPartCopy: simulate S3 500 error
	// The path in mock S3 for UploadPartCopy is "/abort-bucket/vod/abort_test.mp4"
	mockS3.FailureCodes["/abort-bucket/vod/"+key] = http.StatusInternalServerError

	meta := FileMeta{
		URL:         "http://example.com/test.mp4",
		TotalSize:   64 * 1024,
		ContentType: "video/mp4",
	}

	err = backend.Recompose(ctx, key, 2, meta)
	if err == nil {
		t.Fatalf("Expected Recompose to fail due to injected error")
	}

	// Verify that the multipart upload was cleanly aborted and did NOT leak!
	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected 0 active multipart uploads after recomposition failure, got %d", mockS3.ActiveUploads())
	}
}

func TestS3PrehydrationSkipsAlreadyCachedInS3(t *testing.T) {
	mockS3 := NewMockS3Server("prehydrate-s3-bucket")
	defer mockS3.Close()

	tempDir, err := os.MkdirTemp("", "sc2_s3_prehydrate_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "files"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		CacheRevalidateInterval: 1 * time.Hour,
		StorageBackend:          "s3",
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	defer func() { GlobalConfig = oldConfig }()

	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "prehydrate-s3-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    32 * 1024,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	oldStorage := GlobalStorage
	GlobalStorage = backend
	defer func() { GlobalStorage = oldStorage }()

	InitRegistry()

	var ep2Hits atomic.Int32
	ep2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ep2Server.Close()

	ep2URL := ep2Server.URL + "/episode2.mkv"
	ep3URL := ep2Server.URL + "/episode3.mkv"

	ep2Key, ep2Ext := getCacheKeyAndExt(ep2URL)
	ep2CacheName := ep2Key + ep2Ext

	// Pre-commit episode 2 ONLY in S3 (local CACHE_DIR remains completely empty)
	ep2Part := filepath.Join(GlobalConfig.TmpDir, "ep2_seed.part")
	_ = os.WriteFile(ep2Part, []byte("episode2-already-in-s3"), 0644)
	err = backend.CommitLocalFile(context.Background(), ep2CacheName, ep2Part, FileMeta{
		URL:           ep2URL,
		TotalSize:     int64(len("episode2-already-in-s3")),
		LastValidated: time.Now().Unix(),
		Completed:     true,
	})
	if err != nil {
		t.Fatalf("CommitLocalFile failed: %v", err)
	}
	_ = os.Remove(ep2Part)

	pm := &PrehydrationManager{
		queue:               make([]*PrehydrationTask, 0),
		activePrehydrations: make(map[string]bool),
		notifyCh:            make(chan struct{}, 1),
		stopCh:              make(chan struct{}),
	}

	// 1. Enqueue episode 2 (cached in S3) with next episode 3 (not cached) -> should skip ep2 and queue ep3
	pm.Enqueue(ep2URL, []string{ep3URL})

	pm.mu.Lock()
	if len(pm.queue) != 1 || pm.queue[0].TargetURL != ep3URL {
		pm.mu.Unlock()
		t.Fatalf("Expected only %s in prehydration queue after skipping S3-cached ep2, got %+v", ep3URL, pm.queue)
	}
	pm.mu.Unlock()

	// 2. executePrehydration on ep2 must also skip without probing upstream
	pm.executePrehydration(&PrehydrationTask{
		TargetURL: ep2URL,
		NextURLs:  nil,
	})

	if ep2Hits.Load() != 0 {
		t.Errorf("Expected 0 upstream HTTP requests for S3-cached episode2, got %d", ep2Hits.Load())
	}
}

type delayedRecomposeStorage struct {
	*S3StorageBackend
	recomposeStarted chan struct{}
	releaseRecompose chan struct{}
	once             sync.Once
}

func (d *delayedRecomposeStorage) Recompose(ctx context.Context, key string, totalParts int, meta FileMeta) error {
	d.once.Do(func() {
		close(d.recomposeStarted)
	})
	<-d.releaseRecompose
	return d.S3StorageBackend.Recompose(ctx, key, totalParts, meta)
}

func TestS3SynchronousFinalizeAndReaderTransition(t *testing.T) {
	mockS3 := NewMockS3Server("sync-commit-bucket")
	defer mockS3.Close()

	tempDir, err := os.MkdirTemp("", "sc2_s3_sync_commit_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "files"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		CacheRevalidateInterval: 1 * time.Hour,
		WritebackBufferSize:     64 * 1024,
		StorageBackend:          "s3",
		S3Config: S3Config{
			ChunkSize:           32 * 1024,
			ScratchMaxBytes:     10 * 1024 * 1024,
			ScratchMinFreeBytes: 1024,
		},
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	defer func() { GlobalConfig = oldConfig }()

	rawBackend, err := NewS3StorageBackend(S3Config{
		Endpoint:            mockS3.URL(),
		Bucket:              "sync-commit-bucket",
		Region:              "us-east-1",
		AccessKey:           "test-access",
		SecretKey:           "test-secret",
		UsePathStyle:        true,
		DisableSSL:          true,
		ChunkSize:           32 * 1024,
		ScratchMaxBytes:     10 * 1024 * 1024,
		ScratchMinFreeBytes: 1024,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	delayed := &delayedRecomposeStorage{
		S3StorageBackend: rawBackend,
		recomposeStarted: make(chan struct{}),
		releaseRecompose: make(chan struct{}),
	}

	oldStorage := GlobalStorage
	GlobalStorage = delayed
	defer func() { GlobalStorage = oldStorage }()

	InitRegistry()

	key := "sync_commit_episode.mkv"
	totalSize := int64(96 * 1024) // 3 chunks of 32 KB
	fullData := make([]byte, totalSize)
	for i := range fullData {
		fullData[i] = byte((i * 13) % 256)
	}

	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	metaPath := filepath.Join(GlobalConfig.CacheDir, key+".json")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	_ = f.Truncate(totalSize)

	state := NewFileCacheState(key, "http://example.com/sync_ep.mkv", filepath.Join(GlobalConfig.CacheDir, key), partPath, metaPath, "video/x-matroska", "", "", totalSize)
	state.File = f
	state.DiskMaterialized = true
	state.WriterActive = true
	state.UpstreamStatus = "Downloading"
	wp := NewWritebackPipeline(f, state, GlobalConfig.GetWritebackBufferSize())
	state.Pipeline = wp

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// Active viewer streaming during download & commit
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := NewCacheReader(ctx, state, nil)
	defer reader.Close()

	// Simulate streaming all 3 chunks through WritebackPipeline
	for off := int64(0); off < totalSize; off += 16 * 1024 {
		end := off + 16*1024
		if err := wp.WriteChunk(off, fullData[off:end]); err != nil {
			t.Fatalf("WriteChunk error: %v", err)
		}
		state.RangeSet.Add(off, end)
		atomic.AddInt64(&state.TotalBytesDownloaded, end-off)
		GlobalRegistry.UpdateSize(end - off)
	}

	// Simulate coordinator finishing download and starting finalizeAndCommitCache asynchronously
	state.Lock.Lock()
	fileToClose := state.File
	state.File = nil
	pipelineToClose := state.Pipeline
	state.Pipeline = nil
	state.IsCommitting = true
	state.UpstreamStatus = "Committing"
	state.Lock.Unlock()

	commitDone := make(chan struct{})
	go func() {
		finalizeAndCommitCache(state, fileToClose, pipelineToClose)
		close(commitDone)
	}()

	// Wait until Recompose is in-flight (blocked on delayed.releaseRecompose)
	select {
	case <-delayed.recomposeStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for Recompose to start")
	}

	// CRITICAL ASSERTIONS WHILE RECOMPOSE IS IN PROGRESS:
	// 1. State must STILL remain in GlobalRegistry.Files with "Committing" status
	GlobalRegistry.Lock.RLock()
	activeState, stillRegistered := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.RUnlock()
	if !stillRegistered || activeState != state {
		t.Fatalf("Expected %s to remain in GlobalRegistry.Files while S3 Recompose is still running", key)
	}

	// 2. Local .part must NOT be removed yet while Recompose is running
	if !FileExists(partPath) {
		t.Fatalf("Expected scratch part file to remain intact until S3 Recompose completes")
	}

	// 3. Reader must still be able to read chunk 0 from S3 chunks while Recompose is blocked
	bufChunk0 := make([]byte, 16*1024)
	n, err := reader.ReadAt(bufChunk0, 0)
	if err != nil || n != len(bufChunk0) || !bytes.Equal(bufChunk0, fullData[:16*1024]) {
		t.Fatalf("Expected active reader to read chunk 0 from S3 during recomposition (n=%d, err=%v)", n, err)
	}

	// Now allow Recompose to finish sealing vod/<key>
	close(delayed.releaseRecompose)

	select {
	case <-commitDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for finalizeAndCommitCache to finish")
	}

	// POST-COMMIT ASSERTIONS:
	// 1. Part file cleaned up after seal
	if FileExists(partPath) {
		t.Errorf("Expected scratch part file to be removed after S3 Recompose succeeded")
	}

	// 2. State removed from GlobalRegistry.Files and local cache size reconciled back to 0
	GlobalRegistry.Lock.RLock()
	_, stillExists := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.RUnlock()
	if stillExists {
		t.Errorf("Expected %s to be removed from GlobalRegistry.Files after S3 seal", key)
	}
	if totalSizeTracked := GlobalRegistry.GetTotalSize(); totalSizeTracked != 0 {
		t.Errorf("Expected local TotalCacheSize to return to 0 after S3 commit, got %d", totalSizeTracked)
	}

	// 3. Monolithic object exists via Stat, and existing reader transitions seamlessly to monolithic s3Reader
	_, statExists, err := rawBackend.Stat(context.Background(), key)
	if err != nil || !statExists {
		t.Fatalf("Expected monolithic vod/%s to exist via Stat after commit (err=%v, exists=%v)", key, err, statExists)
	}

	bufTail := make([]byte, 32*1024)
	nTail, err := reader.ReadAt(bufTail, 64*1024)
	if err != nil || nTail != len(bufTail) || !bytes.Equal(bufTail, fullData[64*1024:]) {
		t.Fatalf("Expected reader to transition cleanly to monolithic vod/%s (n=%d, err=%v)", key, nTail, err)
	}

	reader.s3ReaderMu.Lock()
	hasS3Reader := reader.s3Reader != nil
	reader.s3ReaderMu.Unlock()
	if !hasS3Reader {
		t.Errorf("Expected reader to have transitioned to s3Reader after S3 recomposition completed")
	}
}

func TestS3StatDoesNotPolluteOriginETag(t *testing.T) {
	mockS3 := NewMockS3Server("etag-bucket")
	defer mockS3.Close()

	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "etag-bucket",
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    32 * 1024,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "sc2_etag_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	key := "no_origin_etag.mkv"
	partPath := filepath.Join(tempDir, key+".part")
	_ = os.WriteFile(partPath, []byte("test-media-bytes"), 0644)

	// Commit without an upstream ETag
	err = backend.CommitLocalFile(context.Background(), key, partPath, FileMeta{
		URL:           "http://example.com/no_origin_etag.mkv",
		TotalSize:     16,
		ETag:          "",
		LastValidated: time.Now().Unix(),
		Completed:     true,
	})
	if err != nil {
		t.Fatalf("CommitLocalFile error: %v", err)
	}

	meta, exists, err := backend.Stat(context.Background(), key)
	if err != nil || !exists {
		t.Fatalf("Stat failed: exists=%v err=%v", exists, err)
	}
	if meta.ETag != "" {
		t.Errorf("Expected meta.ETag to stay empty when upstream ETag is absent, got %q (S3 ETag leaked!)", meta.ETag)
	}
}




