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

	// 4. Initialize Cache Registry (scans directory, calculates cache size, sets bounds)
	InitRegistry()

	// 5. Initialize Prehydration Manager (Dispatcharr capacity awareness & next episode prehydration)
	InitPrehydrationManager()

	// 6. Start HTTP Servers (Media streams and Dashboard UI)
	StartServer()
}
