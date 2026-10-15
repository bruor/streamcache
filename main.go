package main

import (
	"log"
)

func main() {
	log.Println("[Main] Initializing Streamcache VOD Caching Proxy...")

	// 1. Load configuration from environment variables
	LoadConfig()

	// 2. Initialize HTTP Client with cookies and custom redirect handler
	InitHTTPClient()

	// 3. Initialize Log Broker (redirects standard logs to console and SSE listeners)
	InitLogBroker()

	// 4. Initialize Storage Backend (Local filesystem or S3)
	if err := InitStorage(); err != nil {
		log.Fatalf("[Main] Failed to initialize storage backend: %v", err)
	}

	// 5. Initialize Cache Registry (scans directory, calculates cache size, sets bounds)
	InitRegistry()

	// 6. Start HTTP Servers (Media streams and Dashboard UI)
	StartServer()
}
