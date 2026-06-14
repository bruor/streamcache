# Streamcache (SC2) - VOD Caching Proxy

Streamcache is a high-performance HTTP proxy written in Go, specifically designed to optimize Video On Demand (VOD) streaming (such as IPTV streams, large video assets, and static media files). 

It dynamically proxies video requests, downloads and caches content on-demand in blocks, handles byte-range seeking requests efficiently, and implements automated cache eviction and upstream revalidation.

---

## 🚀 Key Features

- **On-Demand Caching with Deduplication**: Media files are cached on-demand when requested by a client. Once triggered, the proxy downloads the media to completion to guarantee a complete local copy. Upstream CDN bandwidth is saved by deduplicating concurrent and subsequent requests for the same media file.
- **Block-Based Starvation Scheduling**: The proxy coordinates downloads in blocks (default 2MB). If a player seeks to a different part of the video, the coordinator instantly shifts priority to download the missing block first. If all active clients (or the only client) are starving, it bypasses connection debouncing rules for an instant jump, ensuring zero playback latency.
- **Full Range Request Support**: Fully compatible with standard HTTP `Range` and `Accept-Ranges` headers. Video players can seek forward or backward smoothly.
- **Smart Cache Eviction (LRU)**: Automatically frees cache space in real-time as chunks are downloaded when the cache size reaches the maximum threshold, or if the host machine's physical disk space drops below a safe limit.
- **Incomplete Download Garbage Collection**: If a background download is aborted or fails during runtime, the proxy automatically cleans up the incomplete `.part` file, deletes the companion JSON metadata, and clears the file from memory as soon as all clients disconnect, reclaiming disk space instantly.
- **Upstream Connection Read Timeout & Tempered Retries**: Implements socket-level inactivity read timeouts to detect and break hung TCP connections, combined with an exponential backoff retry cycle (starting at `2s`, scaling to `30s`), an immediate exit policy for permanent CDN errors (`403`, `404`, `410`), and an 8-consecutive-failures cap to prevent connection spamming.
- **Upstream Cache Revalidation**: Periodically queries the upstream CDN (e.g. hourly) to check if the video file has been updated (via `ETag`/`Last-Modified`). If the upstream file changes, the local cache is automatically invalidated.
- **Dynamic Metadata & Legacy Migration Recovery**: 
  - *Legacy Cache Migration*: Automatically detects and migrates legacy cache files (extensionless `[key]` format) to the new cache standard (`[key].ext` and companion `.json` metadata file) on-demand when requested by a client.
  - *Missing Metadata Recovery*: If a cached media file's companion `.json` metadata file is missing or deleted, the proxy dynamically regenerates it using the file's size and original disk modification time (`ModTime`) rather than evicting the file, ensuring seamless integration with the revalidation pipeline with zero redundant downloads.
- **Dual Proxy Formats**: Supports query parameters (`/stream?url=...`) as well as clean, Base64-encoded URL paths (`/u/<base64_url>`).
- **SSE Web Dashboard**: Includes a real-time web UI showing cache metrics, network stats, real-time CDN download speeds, smart log console scroll-locking, and active client connection details (tracking connection metadata, byte offset, and playback position percentage for downloaded and hydrating streams).

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
| `SCHEDULER_MIN_BYTES` | `16MB` | Minimum bytes to download before the client starvation coordinator switches targets. Supports size units (e.g. `16MB`). Set to `0` or `none` to disable. |
| `SCHEDULER_MIN_TIME` | `2s` | Minimum duration to stream a block before switching targets. Set to `0` or `none` to disable. |
| `UPSTREAM_READ_TIMEOUT` | `30s` | Read timeout for upstream connections. Stalled reads from the CDN exceeding this duration are aborted and retried. |

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

- **SSE Web Dashboard**: `http://localhost:8080/` (displays cache storage status, active downloads, system logs, and live client connections with play offset, percentage progress, and downloaded/hydrating status tracking)
- **JSON Registry State**: `http://localhost:8080/debug/state` (exposes memory maps of cache blocks and detailed client connection metadata)
- **Go pprof Profile Endpoint**: `http://localhost:8080/debug/pprof/` (runtime profiling tools)
