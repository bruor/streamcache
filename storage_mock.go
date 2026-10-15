package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MockS3Server provides an in-memory HTTP server implementing S3 API primitives
type MockS3Server struct {
	server       *httptest.Server
	mu           sync.RWMutex
	buckets      map[string]bool
	objects      map[string][]byte                 // "bucket/key" -> content
	metadata     map[string]map[string]string      // "bucket/key" -> metadata headers
	uploads      map[string]map[int][]byte         // uploadId -> (partNumber -> content)
	uploadMetas  map[string]map[string]string      // uploadId -> metadata headers
	uploadKeys   map[string]string                 // uploadId -> "bucket/key"
	uploadSeq    uint64
	FailureCodes map[string]int                    // key/endpoint -> HTTP status code to simulate
}

// NewMockS3Server starts a new in-memory S3 test server
func NewMockS3Server(bucket string) *MockS3Server {
	m := &MockS3Server{
		buckets:      make(map[string]bool),
		objects:      make(map[string][]byte),
		metadata:     make(map[string]map[string]string),
		uploads:      make(map[string]map[int][]byte),
		uploadMetas:  make(map[string]map[string]string),
		uploadKeys:   make(map[string]string),
		FailureCodes: make(map[string]int),
	}
	m.buckets[bucket] = true

	mux := http.NewServeMux()
	mux.HandleFunc("/", m.handleRequest)
	m.server = httptest.NewServer(mux)
	return m
}

func (m *MockS3Server) URL() string {
	return m.server.URL
}

func (m *MockS3Server) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

func (m *MockS3Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check failure injection
	if status, exists := m.FailureCodes[r.URL.Path]; exists && status > 0 {
		http.Error(w, fmt.Sprintf("Injected Error %d", status), status)
		return
	}

	trimmedPath := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(trimmedPath, "/", 2)
	bucket := parts[0]
	key := ""
	if len(parts) > 1 {
		key = parts[1]
	}

	fullKey := bucket + "/" + key

	// Route based on query params and method
	query := r.URL.Query()

	switch r.Method {
	case http.MethodHead:
		content, ok := m.objects[fullKey]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(content)))
		w.Header().Set("Accept-Ranges", "bytes")
		if meta, ok := m.metadata[fullKey]; ok {
			for k, v := range meta {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		content, ok := m.objects[fullKey]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(content)))
		if meta, ok := m.metadata[fullKey]; ok {
			for k, v := range meta {
				w.Header().Set(k, v)
			}
		}

		// Handle byte range header
		rangeHeader := r.Header.Get("Range")
		if rangeHeader != "" && strings.HasPrefix(rangeHeader, "bytes=") {
			byteRange := strings.TrimPrefix(rangeHeader, "bytes=")
			rangeParts := strings.Split(byteRange, "-")
			start, _ := strconv.ParseInt(rangeParts[0], 10, 64)
			end := int64(len(content)) - 1
			if len(rangeParts) > 1 && rangeParts[1] != "" {
				if parsedEnd, err := strconv.ParseInt(rangeParts[1], 10, 64); err == nil && parsedEnd < end {
					end = parsedEnd
				}
			}

			if start > end || start >= int64(len(content)) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
				http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}

			subSlice := content[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.Header().Set("Content-Length", strconv.Itoa(len(subSlice)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(subSlice)
			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)

	case http.MethodPut:
		uploadID := query.Get("uploadId")
		partNumberStr := query.Get("partNumber")

		if uploadID != "" && partNumberStr != "" {
			// UploadPart or UploadPartCopy
			partNumber, err := strconv.Atoi(partNumberStr)
			if err != nil {
				http.Error(w, "Invalid partNumber", http.StatusBadRequest)
				return
			}

			copySource := r.Header.Get("x-amz-copy-source")
			var partBytes []byte

			if copySource != "" {
				// Server-side copy
				srcKey := strings.TrimPrefix(copySource, "/")
				unescaped, _ := url.PathUnescape(srcKey)
				srcContent, ok := m.objects[unescaped]
				if !ok {
					http.Error(w, "Copy source not found: "+unescaped, http.StatusNotFound)
					return
				}
				partBytes = make([]byte, len(srcContent))
				copy(partBytes, srcContent)

				m.uploads[uploadID][partNumber] = partBytes
				eTag := fmt.Sprintf("\"%x\"", md5.Sum(partBytes))

				type CopyPartResult struct {
					XMLName      xml.Name `xml:"CopyPartResult"`
					LastModified string   `xml:"LastModified"`
					ETag         string   `xml:"ETag"`
				}
				res := CopyPartResult{
					LastModified: time.Now().UTC().Format(time.RFC3339),
					ETag:         eTag,
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusOK)
				_ = xml.NewEncoder(w).Encode(res)
				return
			}

			// Direct UploadPart
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Read error", http.StatusInternalServerError)
				return
			}
			partBytes = body
			m.uploads[uploadID][partNumber] = partBytes
			eTag := fmt.Sprintf("\"%x\"", md5.Sum(partBytes))
			w.Header().Set("ETag", eTag)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Standard PutObject or CopyObject
		copySource := r.Header.Get("x-amz-copy-source")
		if copySource != "" {
			srcKey := strings.TrimPrefix(copySource, "/")
			unescaped, _ := url.PathUnescape(srcKey)
			srcContent, ok := m.objects[unescaped]
			if !ok {
				http.Error(w, "Copy source not found", http.StatusNotFound)
				return
			}
			data := make([]byte, len(srcContent))
			copy(data, srcContent)
			m.objects[fullKey] = data
		} else {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Read error", http.StatusInternalServerError)
				return
			}
			m.objects[fullKey] = body
		}

		// Extract metadata headers
		meta := make(map[string]string)
		for h, vals := range r.Header {
			if strings.HasPrefix(strings.ToLower(h), "x-amz-meta-") {
				meta[h] = vals[0]
			}
		}
		m.metadata[fullKey] = meta

		w.Header().Set("ETag", fmt.Sprintf("\"%x\"", md5.Sum(m.objects[fullKey])))
		w.WriteHeader(http.StatusOK)

	case http.MethodPost:
		if query.Has("uploads") {
			// CreateMultipartUpload
			m.uploadSeq++
			uploadID := fmt.Sprintf("mock-upload-%d-%d", m.uploadSeq, time.Now().UnixNano())
			m.uploads[uploadID] = make(map[int][]byte)
			m.uploadKeys[uploadID] = fullKey

			meta := make(map[string]string)
			for h, vals := range r.Header {
				if strings.HasPrefix(strings.ToLower(h), "x-amz-meta-") {
					meta[h] = vals[0]
				}
			}
			m.uploadMetas[uploadID] = meta

			type InitiateMultipartUploadResult struct {
				XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
				Bucket   string   `xml:"Bucket"`
				Key      string   `xml:"Key"`
				UploadId string   `xml:"UploadId"`
			}
			res := InitiateMultipartUploadResult{
				Bucket:   bucket,
				Key:      key,
				UploadId: uploadID,
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_ = xml.NewEncoder(w).Encode(res)
			return
		}

		uploadID := query.Get("uploadId")
		if uploadID != "" {
			// CompleteMultipartUpload
			partsMap, ok := m.uploads[uploadID]
			if !ok {
				http.Error(w, "Upload not found", http.StatusNotFound)
				return
			}

			type CompletedPart struct {
				PartNumber int    `xml:"PartNumber"`
				ETag       string `xml:"ETag"`
			}
			type CompleteMultipartUpload struct {
				Parts []CompletedPart `xml:"Part"`
			}

			var req CompleteMultipartUpload
			if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "Invalid XML", http.StatusBadRequest)
				return
			}

			var combined []byte
			for _, p := range req.Parts {
				chunkData, exists := partsMap[p.PartNumber]
				if !exists {
					http.Error(w, fmt.Sprintf("Part %d not found", p.PartNumber), http.StatusBadRequest)
					return
				}
				combined = append(combined, chunkData...)
			}

			targetKey := m.uploadKeys[uploadID]
			m.objects[targetKey] = combined
			m.metadata[targetKey] = m.uploadMetas[uploadID]

			delete(m.uploads, uploadID)
			delete(m.uploadKeys, uploadID)
			delete(m.uploadMetas, uploadID)

			eTag := fmt.Sprintf("\"%x\"", md5.Sum(combined))
			type CompleteMultipartUploadResult struct {
				XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
				Location string   `xml:"Location"`
				Bucket   string   `xml:"Bucket"`
				Key      string   `xml:"Key"`
				ETag     string   `xml:"ETag"`
			}
			res := CompleteMultipartUploadResult{
				Location: "/" + targetKey,
				Bucket:   bucket,
				Key:      key,
				ETag:     eTag,
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_ = xml.NewEncoder(w).Encode(res)
			return
		}

		if query.Has("delete") {
			// DeleteObjects (bulk delete)
			type DeleteObject struct {
				Key string `xml:"Key"`
			}
			type DeleteRequest struct {
				Objects []DeleteObject `xml:"Object"`
			}

			var delReq DeleteRequest
			if err := xml.NewDecoder(r.Body).Decode(&delReq); err == nil {
				for _, obj := range delReq.Objects {
					objFullKey := bucket + "/" + obj.Key
					delete(m.objects, objFullKey)
					delete(m.metadata, objFullKey)
				}
			}

			type Deleted struct {
				Key string `xml:"Key"`
			}
			type DeleteResult struct {
				XMLName xml.Name  `xml:"DeleteResult"`
				Deleted []Deleted `xml:"Deleted"`
			}
			var res DeleteResult
			for _, obj := range delReq.Objects {
				res.Deleted = append(res.Deleted, Deleted{Key: obj.Key})
			}
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusOK)
			_ = xml.NewEncoder(w).Encode(res)
			return
		}

	case http.MethodDelete:
		delete(m.objects, fullKey)
		delete(m.metadata, fullKey)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (m *MockS3Server) GetObject(fullKey string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objects[fullKey]
	return b, ok
}

func (m *MockS3Server) ObjectCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

func computeMD5(data []byte) string {
	h := md5.Sum(data)
	return hex.EncodeToString(h[:])
}
