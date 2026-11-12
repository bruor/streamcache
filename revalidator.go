package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var contentRangeRegex = regexp.MustCompile(`/(\d+)\s*$`)

// GlobalInFlightProbes tracks active metadata probe HTTP requests in probeUpstream.
var GlobalInFlightProbes atomic.Int32

// Revalidate checks if a cached VOD has expired and probes the origin
// to see if the file changed on the CDN.
// Returns:
//   - bool: true if cache is valid (serve it), false if cache is invalid/evicted (must re-download)
//   - error: error if critical validation pipeline fails
func Revalidate(key string, url string, playerHeader http.Header) (bool, error) {
	filePath := filepath.Join(GlobalConfig.GetCacheDir(), key)
	metaPath := filePath + ".json"

	var meta FileMeta
	var found bool

	if GlobalStorage != nil {
		sm, exists, err := GlobalStorage.Stat(context.Background(), key)
		if err == nil && exists && sm != nil {
			meta = *sm
			found = true
		}
	}

	if !found {
		// If cached file doesn't exist, we consider it valid (we'll start a fresh download)
		if !FileExists(filePath) {
			return true, nil
		}

		// Read metadata
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			// Video file exists but metadata is missing. Recreate it dynamically based on the file's ModTime.
			log.Printf("[Revalidate] Metadata missing for cached file %s. Recreating from disk stat...", key)
			err = recreateMetadata(filePath, metaPath, url)
			if err != nil {
				log.Printf("[Revalidate] Failed to recreate metadata for %s: %v. Evicting.", key, err)
				evictCachedFile(filePath, metaPath)
				return false, nil
			}
			// Read it back to continue validation
			metaBytes, err = os.ReadFile(metaPath)
			if err != nil {
				evictCachedFile(filePath, metaPath)
				return false, nil
			}
		}

		err = json.Unmarshal(metaBytes, &meta)
		if err != nil {
			log.Printf("[Revalidate] Metadata corrupted for cached file %s. Evicting.", key)
			evictCachedFile(filePath, metaPath)
			return false, nil
		}
	}

	// Check if the validation time has expired
	age := time.Since(time.Unix(meta.LastValidated, 0))
	if age < GlobalConfig.GetCacheRevalidateInterval() {
		// Fresh validation, skip network probe
		return true, nil
	}

	log.Printf("[Revalidate] Cache validation expired for %s (Age: %s). Probing CDN upstream...", key, age.Truncate(time.Second))

	upstreamSize, _, upstreamETag, upstreamLastModified, notModified, err := probeUpstream(context.Background(), url, playerHeader, &meta)
	if err != nil {
		log.Printf("[Revalidate] Network probe failed for %s: %v. Soft-failing and serving cache.", key, err)
		return true, nil
	}

	if notModified {
		log.Printf("[Revalidate] CDN returned 304 Not Modified. Cache is valid for %s", key)
	} else {
		// Detect modifications
		modified := false
		reason := ""

		// Only validate size if it is greater than 1. An upstreamSize of 1 or 0 usually indicates
		// a parsed Content-Length of the single-byte range request due to a missing Content-Range header.
		if upstreamSize > 1 && upstreamSize != meta.TotalSize {
			modified = true
			reason = fmt.Sprintf("file size changed (cached: %d, upstream: %d)", meta.TotalSize, upstreamSize)
		} else if upstreamETag != "" && meta.ETag != "" && upstreamETag != meta.ETag {
			modified = true
			reason = fmt.Sprintf("ETag changed (cached: %s, upstream: %s)", meta.ETag, upstreamETag)
		} else if upstreamLastModified != "" && meta.LastModified != "" && upstreamLastModified != meta.LastModified {
			modified = true
			reason = fmt.Sprintf("Last-Modified changed (cached: %s, upstream: %s)", meta.LastModified, upstreamLastModified)
		}

		if modified {
			log.Printf("[Revalidate] ALERT: Media content changed on CDN upstream: %s. Evicting cache for %s.", reason, key)
			evictCachedFile(filePath, metaPath)
			return false, nil
		}
	}

	// Cache is valid. Update Last-Validated timestamp.
	meta.LastValidated = time.Now().Unix()
	if !notModified {
		if upstreamETag != "" {
			meta.ETag = upstreamETag
		}
		if upstreamLastModified != "" {
			meta.LastModified = upstreamLastModified
		}
	}
	if GlobalStorage != nil {
		_ = GlobalStorage.UpdateMetadata(context.Background(), key, meta)
	}
	newMetaBytes, err := json.Marshal(meta)
	if err == nil {
		_ = writeMetaFileAtomic(metaPath, newMetaBytes)
	}

	log.Printf("[Revalidate] Cache validated successfully. Serving HIT for: %s", key)
	return true, nil
}

func writeMetaFileAtomic(metaPath string, data []byte) error {
	tmpPath := fmt.Sprintf("%s.tmp.%d", metaPath, time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return os.WriteFile(metaPath, data, 0644)
	}
	if err := os.Rename(tmpPath, metaPath); err != nil {
		_ = os.Remove(tmpPath)
		return os.WriteFile(metaPath, data, 0644)
	}
	return nil
}

func evictCachedFile(path, metaPath string) {
	key := filepath.Base(path)
	if GlobalStorage != nil {
		_ = GlobalStorage.Delete(context.Background(), key)
	}

	info, err := os.Stat(path)
	var size int64
	if err == nil {
		size = info.Size()
	}

	if size > 0 {
		GlobalRegistry.UpdateSize(-size)
	}

	go func() {
		_ = os.Remove(path)
		_ = os.Remove(metaPath)
	}()
}

func parseTotalSizeFromHeaders(header http.Header) (int64, error) {
	// 1. Try Content-Range
	contentRange := header.Get("Content-Range")
	if contentRange != "" {
		matches := contentRangeRegex.FindStringSubmatch(contentRange)
		if len(matches) > 1 {
			size, err := strconv.ParseInt(matches[1], 10, 64)
			if err == nil {
				return size, nil
			}
		}
	}

	// 2. Fall back to Content-Length (only valid if Status was 200, which bytes=0-0 range GET would not be unless CDN ignored range)
	contentLength := header.Get("Content-Length")
	if contentLength != "" {
		size, err := strconv.ParseInt(contentLength, 10, 64)
		if err == nil {
			return size, nil
		}
	}

	return 0, fmt.Errorf("unable to determine total size from headers")
}

func recreateMetadata(filePath, metaPath, url string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	meta := FileMeta{
		URL:           url,
		TotalSize:     stat.Size(),
		LastValidated: stat.ModTime().Unix(),
		Completed:     true,
	}

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return writeMetaFileAtomic(metaPath, metaBytes)
}

// probeUpstream attempts to determine the file's size, Content-Type, ETag, and Last-Modified headers.
func probeUpstream(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, string, bool, error) {
	GlobalInFlightProbes.Add(1)
	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
	defer func() {
		GlobalInFlightProbes.Add(-1)
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}
	}()

	originalHost := GetCleanHost(targetURL)
	preferredMethod := GetProbeMethod(originalHost)

	log.Printf("[Probe] Probing %s (Host: %s, Preferred Method: %s)", targetURL, originalHost, preferredMethod)

	var lastErr error

	// If preferred method is range, try Range: bytes=0-0 first
	if preferredMethod == ProbeMethodRange {
		size, ctype, etag, lm, notModified, err := tryRangeProbe(ctx, targetURL, playerHeader, meta)
		if err == nil {
			SetProbeMethod(originalHost, ProbeMethodRange)
			return size, ctype, etag, lm, notModified, nil
		}
		if isCatastrophicProbeError(err) {
			desc := describeCatastrophicProbeError(err)
			log.Printf("[Probe] Range probe hit catastrophic failure on host %s [%s]: %v. Aborting immediately.", originalHost, desc, err)
			return 0, "", "", "", false, err
		}
		log.Printf("[Probe] Range probe failed for host %s: %v. Trying standard GET fallback...", originalHost, err)
		lastErr = err
	}

	// Try standard GET fallback (or if standard GET is preferred)
	size, ctype, etag, lm, notModified, err := tryStandardProbe(ctx, targetURL, playerHeader, meta)
	if err == nil {
		SetProbeMethod(originalHost, ProbeMethodStandard)
		return size, ctype, etag, lm, notModified, nil
	}
	if isCatastrophicProbeError(err) {
		desc := describeCatastrophicProbeError(err)
		log.Printf("[Probe] Standard probe hit catastrophic failure on host %s [%s]: %v. Aborting immediately.", originalHost, desc, err)
		return 0, "", "", "", false, err
	}

	// If standard GET failed but we haven't tried Range probe yet, try Range probe as final fallback
	if preferredMethod == ProbeMethodStandard {
		log.Printf("[Probe] Standard probe failed for host %s: %v. Trying Range probe fallback...", originalHost, err)
		size, ctype, etag, lm, notModified, rerr := tryRangeProbe(ctx, targetURL, playerHeader, meta)
		if rerr == nil {
			SetProbeMethod(originalHost, ProbeMethodRange)
			return size, ctype, etag, lm, notModified, nil
		}
		lastErr = rerr
	} else {
		lastErr = err
	}

	return 0, "", "", "", false, fmt.Errorf("all probe methods failed; last error: %w", lastErr)
}

func tryRangeProbe(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, string, bool, error) {
	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()

	req, err := http.NewRequestWithContext(probeCtx, "GET", targetURL, nil)
	if err != nil {
		return 0, "", "", "", false, err
	}

	req.Header.Set("Range", "bytes=0-0")
	CopySafeHeaders(playerHeader, req.Header)

	// Set conditional headers if metadata is present
	if meta != nil {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	resp, err := UpstreamHTTPClient.Do(req)
	if err != nil {
		return 0, "", "", "", false, err
	}
	defer func() {
		cancelProbe()
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return 0, "", resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, "", "", "", false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return 0, "", "", "", false, fmt.Errorf("upstream returned HTML content type %q (likely a CDN block or redirect)", contentType)
	}

	// Cache successful target host
	finalHost := GetCleanHost(resp.Request.URL.String())
	if finalHost != "" {
		SetProbeMethod(finalHost, ProbeMethodRange)
	}

	size, err := parseTotalSizeFromHeaders(resp.Header)
	if err != nil {
		return 0, "", "", "", false, err
	}

	return size, contentType, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}

func tryStandardProbe(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, string, bool, error) {
	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()

	req, err := http.NewRequestWithContext(probeCtx, "GET", targetURL, nil)
	if err != nil {
		return 0, "", "", "", false, err
	}

	CopySafeHeaders(playerHeader, req.Header)

	// Set conditional headers if metadata is present
	if meta != nil {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	resp, err := UpstreamHTTPClient.Do(req)
	if err != nil {
		return 0, "", "", "", false, err
	}
	defer func() {
		cancelProbe()
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotModified {
		return 0, "", resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}

	if resp.StatusCode != http.StatusOK {
		return 0, "", "", "", false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(strings.ToLower(contentType), "text/html") {
		return 0, "", "", "", false, fmt.Errorf("upstream returned HTML content type %q (likely a CDN block or redirect)", contentType)
	}

	// Cache successful target host
	finalHost := GetCleanHost(resp.Request.URL.String())
	if finalHost != "" {
		SetProbeMethod(finalHost, ProbeMethodStandard)
	}

	// For standard GET, the size is returned in the Content-Length header
	contentLengthStr := resp.Header.Get("Content-Length")
	if contentLengthStr == "" {
		// Fall back to checking Content-Range just in case the server responded with 206 or Content-Range
		contentRange := resp.Header.Get("Content-Range")
		if contentRange != "" {
			matches := contentRangeRegex.FindStringSubmatch(contentRange)
			if len(matches) > 1 {
				size, err := strconv.ParseInt(matches[1], 10, 64)
				if err == nil {
					return size, contentType, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
				}
			}
		}
		return 0, "", "", "", false, fmt.Errorf("Content-Length header is missing in standard probe")
	}

	size, err := strconv.ParseInt(contentLengthStr, 10, 64)
	if err != nil {
		return 0, "", "", "", false, fmt.Errorf("invalid Content-Length value %q: %w", contentLengthStr, err)
	}

	return size, contentType, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}

func isHTMLProbeError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "upstream returned HTML content type")
}

func isPermanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unexpected status code: 403") ||
		strings.Contains(msg, "unexpected status code: 404") ||
		strings.Contains(msg, "unexpected status code: 410")
}

func isCatastrophicProbeError(err error) bool {
	if err == nil {
		return false
	}
	if isHTMLProbeError(err) {
		return true
	}
	// Check for permanent errors except 403 (like 404, 410)
	if isPermanentProbeError(err) && !strings.Contains(err.Error(), "unexpected status code: 403") {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "dial tcp")
}

func describeCatastrophicProbeError(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	if isHTMLProbeError(err) {
		return "HTML block page returned (CDN challenge or redirect)"
	}
	if isPermanentProbeError(err) {
		return "permanent HTTP error (file not found/expired upstream)"
	}
	if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "timeout awaiting response headers") {
		return "upstream response header timeout (remote storage or gateway latency exceeded deadline)"
	}
	if strings.Contains(msg, "connection refused") {
		return "connection refused (upstream host closed or rejecting traffic)"
	}
	if strings.Contains(msg, "no such host") {
		return "DNS resolution failure"
	}
	if strings.Contains(msg, "dial tcp") {
		return "TCP dial/handshake failure"
	}
	return "unrecoverable transport/network failure"
}
