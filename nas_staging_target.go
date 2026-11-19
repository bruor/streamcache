package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// renameFileFn is injectable in tests to deterministically simulate syscall.EXDEV.
var renameFileFn = os.Rename

// moveFileCrossFS moves src to dst via atomic rename when on the same filesystem,
// falling back on syscall.EXDEV to streaming copy via dst+".staging" -> atomic rename -> remove src.
func moveFileCrossFS(src, dst string) error {
	err := renameFileFn(src, dst)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	stagingPath := dst + ".staging"
	dstFile, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = dstFile.Close()
			_ = os.Remove(stagingPath)
		}
	}()

	const bufSize = 256 * 1024
	buf := make([]byte, bufSize)
	var written int64
	const adviseWindow = 8 * 1024 * 1024
	var lastAdvised int64

	for {
		n, rerr := srcFile.Read(buf)
		if n > 0 {
			if _, werr := dstFile.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written-lastAdvised >= adviseWindow {
				FadviseDontNeed(dstFile.Fd(), lastAdvised, written-lastAdvised)
				FadviseDontNeed(srcFile.Fd(), lastAdvised, written-lastAdvised)
				lastAdvised = written
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	if written > lastAdvised {
		FadviseDontNeed(dstFile.Fd(), lastAdvised, written-lastAdvised)
	}
	FadviseDontNeed(srcFile.Fd(), 0, 0)

	if err := dstFile.Close(); err != nil {
		return err
	}
	cleanupStaging = false

	if err := renameFileFn(stagingPath, dst); err != nil {
		_ = os.Remove(stagingPath)
		return err
	}

	_ = os.Remove(src)
	return nil
}

// NASStagingTarget implements ChunkTarget for cross-filesystem TMP_DIR -> CACHE_DIR staging.
// Chunks are copied into CACHE_DIR/<key>.staging before TMP_DIR/<key>.part holes are punched,
// and Commit atomically renames <key>.staging to the final cache path without EXDEV copies.
type NASStagingTarget struct {
	cacheDir    string
	key         string
	sequential  bool
	concurrency int
	stagingPath string
	finalPath   string

	mu          sync.RWMutex
	stagingFile *os.File
	closed      bool
}

// NewNASStagingTarget constructs a ChunkTarget writing chunks to CACHE_DIR/<key>.staging.
func NewNASStagingTarget(cacheDir, key string, sequential bool) *NASStagingTarget {
	conc := 2
	if sequential {
		conc = 1
	}
	finalPath := filepath.Join(cacheDir, key)
	return &NASStagingTarget{
		cacheDir:    cacheDir,
		key:         key,
		sequential:  sequential,
		concurrency: conc,
		stagingPath: finalPath + ".staging",
		finalPath:   finalPath,
	}
}

func (t *NASStagingTarget) Name() string {
	return "nas"
}

func (t *NASStagingTarget) IsSequential() bool {
	return t.sequential
}

func (t *NASStagingTarget) Concurrency() int {
	if t.sequential {
		return 1
	}
	if t.concurrency <= 0 {
		return 2
	}
	return t.concurrency
}

func (t *NASStagingTarget) SupportsMultipart(totalSize, configuredChunkSize int64) (int64, bool) {
	if configuredChunkSize <= 0 {
		configuredChunkSize = 16 * 1024 * 1024
	}
	return configuredChunkSize, true
}

func (t *NASStagingTarget) ensureStagingFileLocked() (*os.File, error) {
	if t.closed {
		return nil, os.ErrClosed
	}
	if t.stagingFile != nil {
		return t.stagingFile, nil
	}
	if err := os.MkdirAll(t.cacheDir, 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(t.stagingPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	t.stagingFile = f
	return f, nil
}

func (t *NASStagingTarget) PushChunk(ctx context.Context, key string, chunkIndex int, chunkStart int64, r io.Reader, size int64) error {
	t.mu.Lock()
	f, err := t.ensureStagingFileLocked()
	t.mu.Unlock()
	if err != nil {
		return err
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed || t.stagingFile != f {
		return os.ErrClosed
	}

	buf := make([]byte, 256*1024)
	var written int64
	for {
		if ctx != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		toRead := len(buf)
		if size > 0 {
			rem := size - written
			if rem <= 0 {
				break
			}
			if int64(toRead) > rem {
				toRead = int(rem)
			}
		}
		n, rerr := r.Read(buf[:toRead])
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], chunkStart+written); werr != nil {
				return werr
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	if size > 0 && written < size {
		return io.ErrUnexpectedEOF
	}
	if written > 0 {
		FadviseDontNeed(f.Fd(), chunkStart, written)
	}
	return nil
}

func (t *NASStagingTarget) Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error {
	if onProgress != nil {
		onProgress("finalizing", 0, 1)
	}

	t.mu.Lock()
	t.closed = true
	if t.stagingFile != nil {
		_ = t.stagingFile.Close()
		t.stagingFile = nil
	}
	t.mu.Unlock()

	if !FileExists(t.finalPath) {
		if FileExists(t.stagingPath) {
			if err := moveFileCrossFS(t.stagingPath, t.finalPath); err != nil {
				return err
			}
		} else if localPartPath != "" && FileExists(localPartPath) {
			if err := moveFileCrossFS(localPartPath, t.finalPath); err != nil {
				return err
			}
		}
	} else {
		_ = os.Remove(t.stagingPath)
	}

	if cf, err := os.Open(t.finalPath); err == nil {
		FadviseDontNeed(cf.Fd(), 0, 0)
		_ = cf.Close()
	}

	meta.Completed = true
	meta.LastValidated = time.Now().Unix()
	if metaBytes, err := json.Marshal(meta); err == nil {
		_ = writeMetaFileAtomic(t.finalPath+".json", metaBytes)
	}

	if onProgress != nil {
		onProgress("finalizing", 1, 1)
	}
	return nil
}

type nasRangeReadCloser struct {
	file   *os.File
	reader *io.SectionReader
}

func (n *nasRangeReadCloser) Read(p []byte) (int, error) {
	return n.reader.Read(p)
}

func (n *nasRangeReadCloser) Close() error {
	return n.file.Close()
}

func (t *NASStagingTarget) ReadCommittedChunk(ctx context.Context, key string, chunkIndex int, chunkStart, offsetInChunk, length int64) (io.ReadCloser, error) {
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
	}

	f, err := os.Open(t.stagingPath)
	if err != nil {
		f, err = os.Open(t.finalPath)
		if err != nil {
			return nil, err
		}
	}

	absOff := chunkStart + offsetInChunk
	return &nasRangeReadCloser{
		file:   f,
		reader: io.NewSectionReader(f, absOff, length),
	}, nil
}

func (t *NASStagingTarget) Cleanup(ctx context.Context, key string) error {
	t.mu.Lock()
	t.closed = true
	f := t.stagingFile
	t.stagingFile = nil
	t.mu.Unlock()

	if !IsNASHealthy() {
		return ErrStorageCircuitOpen
	}
	if f != nil {
		_ = f.Close()
	}

	err := os.Remove(t.stagingPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
