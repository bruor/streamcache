# Streamcache (SC2) - VOD Caching Proxy

Streamcache is a high-performance HTTP proxy written in Go, specifically designed to optimize Video On Demand (VOD) streaming (such as IPTV streams, large video assets, and static media files). 

It dynamically proxies video requests, downloads and caches content on-demand in blocks, handles byte-range seeking requests efficiently, and implements automated cache eviction and upstream revalidation.

---

## 🚀 Key Features

- **On-Demand Caching & Non-Blocking Commits**: Media files are cached on-demand when requested by a client. Once triggered, the proxy downloads the media in parallel to guarantee a complete local copy. Upon completion, file finalization (`.part` to final cache filename) occurs instantly via non-blocking atomic rename (<1ms) without hardware storage barriers (`fsync`), ensuring concurrent downloads run smoothly with zero freezes. Upstream CDN bandwidth is saved by deduplicating concurrent and subsequent requests for the same media file.
- **Concurrent Block Hydration & Starvation Scheduling**: The proxy coordinates media downloads in parallel blocks (configured via `MAX_UPSTREAM_WORKERS`). If a player seeks to a different part of the video, the coordinator instantly shifts priority by spawning or pre-empting download workers to download the starved block first. It features:
  - *Worker Coalescing*: Worker threads automatically terminate if they hit blocks already completed or being actively downloaded by another thread.
  - *Client Hydration Wait*: If an active worker is already downloading a starved block and its estimated completion time (ETA) is under `CLIENT_HYDRATION_WAIT` (default `2s`), the proxy suppresses spawning new connections, saving valuable upstream network resources.
- **Sniff-Targeted Metadata Pre-fetching**: On media startup, the proxy sniffs the first block of the media file to determine if the container is web-optimized. If it is not optimized (i.e. metadata index is at the end of the file), the proxy spawns a temporary parallel helper worker to pre-fetch the exact metadata blocks. This satisfies player probes/indices immediately from cache, preventing connection thrashing and socket resets.
- **Full Range Request Support**: Fully compatible with standard HTTP `Range` and `Accept-Ranges` headers. Video players can seek forward or backward smoothly.
- **Smart Cache Pre-Eviction (LRU)**: Automatically pre-evicts inactive cached files in LRU order at the start of hydration as soon as the media file size is determined, ensuring sufficient disk space and cache quota before downloads begin.
- **Incomplete Download Garbage Collection**: If a background download is aborted or fails during runtime, the proxy automatically cleans up the incomplete `.part` file, deletes the companion JSON metadata, and clears the file from memory as soon as all clients disconnect, reclaiming disk space instantly.
- **Upstream Connection Read Timeout & Tempered Retries**: Implements socket-level inactivity read timeouts to detect and break hung TCP connections, combined with an exponential backoff retry cycle (starting at `2s`, scaling to `30s`), an immediate exit policy for permanent CDN errors (`403`, `404`, `410`), and configurable max retries (`MAX_UPSTREAM_RETRIES`, defaulting to `0` / unlimited retries for high-resilience streaming during backend storage rebalances).
- **Upstream Cache Revalidation**: Periodically queries the upstream CDN (e.g. hourly) to check if the video file has been updated (via `ETag`/`Last-Modified`). If the upstream file changes, the local cache is automatically invalidated.
- **CDN-Friendly Robust Metadata Probing & Adaptive Learning**:
  - Automatically sanitizes player request headers to bypass CDN blocklists and compression conflicts (stripping headers like `Accept-Encoding: gzip` or conditional headers).
  - Uses an adaptive learning algorithm (in-memory thread-safe registry) to discover and cache the optimal probing method (`Range: bytes=0-0` or standard `GET`) per CDN host/IP on the fly.
  - Sequentially falls back from range requests to standard `GET` requests (inspecting headers and closing the body instantly to prevent downloading content) to handle CDNs that block range requests or reject HEAD requests.
  - Leverages HTTP conditional revalidation (`If-None-Match`/`If-Modified-Since`) to return fast `304 Not Modified` responses, conserving upstream bandwidth.
- **Dynamic Metadata & Legacy Migration Recovery**: 
  - *Legacy Cache Migration*: Automatically detects and migrates legacy cache files (extensionless `[key]` format) to the new cache standard (`[key].ext` and companion `.json` metadata file) on-demand when requested by a client.
  - *Missing Metadata Recovery*: If a cached media file's companion `.json` metadata file is missing or deleted, the proxy dynamically regenerates it using the file's size and original disk modification time (`ModTime`) rather than evicting the file, ensuring seamless integration with the revalidation pipeline with zero redundant downloads.
- **Dual Proxy Formats**: Supports query parameters (`/stream?url=...`) as well as clean, Base64-encoded URL paths (`/u/<base64_url>`).
- **SSE Web Dashboard & Contiguous Playback Sessions**: Includes a real-time web UI showing cache metrics, network stats, real-time CDN download speeds (instant EMA-smoothed and overall average), smart log console scroll-locking, and contiguous client playback sessions. It visualizes the real-time connection status between the proxy and the upstream CDN (`Connecting`, `Downloading`, `Throttled` for 0 B/s provider pauses, `Retrying` for worker error backoffs, `Committing` for non-blocking file commits on disk, and `Completed`) to clearly distinguish upstream network conditions from client-side playback starvation.

---

## ⚙️ Configuration Guide

Streamcache is configured using environment variables. Default fallback values are designed for local development.

| Environment Variable | Default | Description |
|---|---|---|
| `PORT` | `:8080` | Port the proxy server listens on. |
| `CACHE_DIR` | `./cache/files` | Location where completed cached media files and JSON metadata are stored. |
| `TMP_DIR` | `./cache/tmp` | Location where active `.part` downloads are assembled. |
| `BLOCK_SIZE` | `2MB` | Size of each block chunk requested from the CDN. Supports size units (e.g. `2MB`, `512KB`). |
| `CACHE_MAX_BYTES` | `50GB` | Maximum size of the cache directory. Set to `0`, `none`, or `off` to disable the limit and rely solely on disk space checks. Supports size units (e.g. `50GB`, `1TB`). |
| `MIN_DISK_FREE_BYTES` | `10GB` | Safety buffer for physical disk space that overrides the cache size limit to prevent disk filling. Supports size units (e.g. `10GB`). |
| `CACHE_REVALIDATE_INTERVAL` | `1h` | Cache expiration age before forcing an HTTP revalidation probe to the source host. |
| `ALLOWED_HOSTS` | *(empty)* | Optional comma-separated list of domain names (e.g. `cdn.example.com`) to restrict which upstreams can be proxied. |
| `SCHEDULER_MIN_BLOCKS` | `4` | Minimum blocks to download before a worker can be pre-empted (protecting CDNs from connection thrashing). Set to `0` to disable. |
| `MAX_PREFETCH_TAILBLOCKS` | `10` | Maximum number of tail blocks to pre-fetch for non-optimized or unsupported media files (protecting upstream network). |
| `SCHEDULER_MIN_TIME` | `2s` | Minimum duration to stream a block before switching targets. Set to `0` or `none` to disable. |
| `UPSTREAM_READ_TIMEOUT` | `0s` (unlimited) | Read timeout for stalled upstream reads. Default is `0s` (unlimited/no timeout), allowing IPTV providers with pause/burst throttling to remain connected. Can be set to a duration (e.g. `30s`, `90s`) to tune down if needed. |
| `MAX_UPSTREAM_RETRIES` | `0` (unlimited) | Maximum consecutive retry attempts per worker before worker self-termination. Default is `0` (unlimited retries). Can be set to a positive integer (e.g. `5`, `8`) to cap retries. |
| `MAX_UPSTREAM_WORKERS` | `2` | Maximum concurrent upstream downloader threads per VOD file. |
| `CLIENT_HYDRATION_WAIT` | `2s` | Duration the proxy waits for active hydration to catch up before spawning a new connection (Jump Suppression). |
| `PROBE_FAILURE_COOLDOWN` | `10m` | Cooldown duration for caching catastrophic/permanent probe failures (e.g. HTML blocks, HTTP 403/404) to block retries. |
| `PROBE_TRANSIENT_COOLDOWN` | `30s` | Cooldown duration for caching transient probe failures (e.g. timeouts, network drops, HTTP 500/503) to absorb retry storms. |
| `UPSTREAM_USER_AGENT_MODE` | `smart` | User-Agent spoofing mode for upstream CDN requests (`smart`, `force`, `passthrough`). `smart` passes valid player UAs while automatically rewriting throttled/bot signatures (e.g. `Lavf`, `FFmpeg`, `Unknown`). `force` always overrides all client UAs. `passthrough` passes client UAs, using default only if empty. |
| `UPSTREAM_USER_AGENT` | `VLC/3.0.20 LibVLC/3.0.20` | Upstream User-Agent string sent to CDNs when spoofing or fallback is applied. |


---

## 🏃 Getting Started & Running the Service

### Run Locally (Go Installed)

1. Clone or download the repository.
2. Download dependencies and run the tests:
   ```bash
   go mod tidy
   go test -v ./...
   ```
3. Build the binary:
   ```bash
   go build -o streamcache
   ```
4. Run the proxy:
   ```bash
   ./streamcache
   ```
   *The server will start listening on port `8080` by default. You can access the real-time SSE Web Dashboard at `http://localhost:8080/`.*

---

### Run with Docker / Docker Compose

If you have Docker installed, the easiest way to start the service is via Docker Compose:

1. Build the image and start the container in the background:
   ```bash
   docker compose up -d
   ```
2. Monitor application logs:
   ```bash
   docker compose logs -f
   ```
3. Stop the container:
   ```bash
   docker compose down
   ```

#### Building from a Private GitHub Repository
If you configure Docker Compose to pull the codebase directly from the remote private GitHub repository, you must authenticate using a GitHub Personal Access Token (PAT):
1. **Create a PAT** with the following scope/permissions:
   * **Fine-Grained PAT**: Select your repository and set `Contents: Read-only`.
   * **Classic PAT**: Select the `repo` scope.
2. **Build and Start**:
   Export the token in your shell environment before running Compose:
   ```bash
   export GITHUB_TOKEN=your_personal_access_token
   docker compose up -d --build
   ```

---

## 📺 How to Use the Proxy

Once Streamcache is running, you can stream media by pointing your video players (e.g. VLC, MPV, Kodi, or HTML5 player) to the proxy using either of the routing methods:

### Method A: Query Parameter Routing
Append the full, URL-encoded video URL to the `/stream` endpoint:
```http
http://localhost:8080/stream?url=https://commondatastorage.googleapis.com/gtv-videos-bucket/sample/BigBuckBunny.mp4
```

### Method B: Base64 URL Routing
For clean URLs or to hide raw upstream links from clients, Base64-encode the target URL (supports standard/URL-safe Base64) and append it to `/u/`:
1. Encode `https://commondatastorage.googleapis.com/gtv-videos-bucket/sample/BigBuckBunny.mp4` to:
   `aHR0cHM6Ly9jb21tb25kYXRhc3RvcmFnZS5nb29nbGVhcGlzLmNvbS9ndHYtdmlkZW9zLWJ1Y2tldC9zYW1wbGUvQmlnQnVja0J1bm55Lm1wNA==`
2. Stream via:
   ```http
   http://localhost:8080/u/aHR0cHM6Ly9jb21tb25kYXRhc3RvcmFnZS5nb29nbGVhcGlzLmNvbS9ndHYtdmlkZW9zLWJ1Y2tldC9zYW1wbGUvQmlnQnVja0J1bm55Lm1wNA==
   ```

---

## 🛠️ Diagnostics & Development

- **SSE Web Dashboard**: `http://localhost:8080/` (displays cache storage status, active downloads, system logs, and stable client playback sessions with play offset, percentage progress, active connection count, and downloaded/hydrating/idle status tracking)
- **JSON Registry State**: `http://localhost:8080/debug/state` (exposes memory maps of cache blocks and detailed client connection metadata)
- **Go pprof Profile Endpoint**: `http://localhost:8080/debug/pprof/` (runtime profiling tools)
