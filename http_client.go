package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

var UpstreamHTTPClient *http.Client

func InitHTTPClient() {
	// Initialize a Cookie Jar to automatically handle Set-Cookie headers in redirects
	jar, err := cookiejar.New(nil)
	if err != nil {
		// Should never happen with nil options
		jar = nil
	}

	// Create custom transport with sensible dial timeouts and TLS configs
	transport := &http.Transport{
		DialContext: (&netDialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		ReadBufferSize:        512 * 1024, // 512KB read buffer for high-bandwidth 1Gbps+ streams
		// Disable TLS certificate verification if required (IPTV providers sometimes use self-signed certs)
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	UpstreamHTTPClient = &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   0, // Unlimited timeout for long-lived streams (handled by Transport timeouts)
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}

			// Copy request headers from the last hop
			lastReq := via[len(via)-1]

			// Explicitly carry over the User-Agent
			if ua := lastReq.Header.Get("User-Agent"); ua != "" {
				req.Header.Set("User-Agent", ua)
			}

			// Copy other common media headers
			headersToCopy := []string{
				"Referer", "Accept", "Accept-Language", 
				"Accept-Encoding", "X-Playback-Session-Id",
			}
			for _, h := range headersToCopy {
				if v := lastReq.Header.Get(h); v != "" {
					req.Header.Set(h, v)
				}
			}

			return nil
		},
	}
}

// Simple struct to implement DialContext
type netDialer struct {
	Timeout   time.Duration
	KeepAlive time.Duration
}

func (d *netDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	var dialer net.Dialer
	dialer.Timeout = d.Timeout
	dialer.KeepAlive = d.KeepAlive
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &timeoutConn{Conn: conn, readTimeout: GlobalConfig.GetUpstreamReadTimeout()}, nil
}

type timeoutConn struct {
	net.Conn
	readTimeout time.Duration
}

func (c *timeoutConn) Read(b []byte) (n int, err error) {
	if c.readTimeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(c.readTimeout))
		defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	}
	return c.Conn.Read(b)
}

type ProbeMethod string

const (
	ProbeMethodRange    ProbeMethod = "range"
	ProbeMethodStandard ProbeMethod = "standard"
)

var (
	probeMethodsMu sync.RWMutex
	probeMethods   = make(map[string]ProbeMethod)
)

func GetProbeMethod(host string) ProbeMethod {
	if host == "" {
		return ProbeMethodRange
	}
	probeMethodsMu.RLock()
	defer probeMethodsMu.RUnlock()
	if method, exists := probeMethods[host]; exists {
		return method
	}
	return ProbeMethodRange
}

func SetProbeMethod(host string, method ProbeMethod) {
	if host == "" {
		return
	}
	probeMethodsMu.Lock()
	defer probeMethodsMu.Unlock()
	probeMethods[host] = method
}

func GetCleanHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := parsed.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host
}

func isThrottledUA(ua string) bool {
	if strings.TrimSpace(ua) == "" {
		return true
	}
	uaLower := strings.ToLower(ua)
	throttledKeywords := []string{
		"lavf", "ffmpeg", "libavformat", "avformat",
		"gstreamer", "libsoup", "stagefright", "androidmediaplayer", "dalvik",
		"curl", "wget", "python", "requests", "libcurl", "go-http-client", "aria2", "httpie",
		"unknown", "internal", "null",
	}
	for _, kw := range throttledKeywords {
		if strings.Contains(uaLower, kw) {
			return true
		}
	}
	return false
}

func CopySafeHeaders(src, dst http.Header) {
	if dst == nil {
		return
	}
	if src != nil {
		for k, vv := range src {
			kLower := strings.ToLower(k)
			if kLower == "range" || kLower == "connection" || kLower == "host" || kLower == "accept-encoding" || strings.HasPrefix(kLower, "if-") || strings.HasPrefix(kLower, "sec-fetch-") {
				continue
			}
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}

	mode := GlobalConfig.GetUpstreamUserAgentMode()
	fallbackUA := GlobalConfig.GetUpstreamUserAgent()

	switch mode {
	case "force":
		dst.Set("User-Agent", fallbackUA)
	case "smart":
		currentUA := dst.Get("User-Agent")
		if isThrottledUA(currentUA) {
			dst.Set("User-Agent", fallbackUA)
		}
	case "passthrough":
		currentUA := dst.Get("User-Agent")
		if strings.TrimSpace(currentUA) == "" || strings.EqualFold(currentUA, "unknown") {
			dst.Set("User-Agent", fallbackUA)
		}
	default:
		currentUA := dst.Get("User-Agent")
		if isThrottledUA(currentUA) {
			dst.Set("User-Agent", fallbackUA)
		}
	}

	if dst.Get("Accept") == "" {
		dst.Set("Accept", "*/*")
	}
}

