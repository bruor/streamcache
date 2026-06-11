package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
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
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
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
	return dialer.DialContext(ctx, network, address)
}
