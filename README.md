# streamcache - VOD Caching Proxy

streamcache is a high-performance HTTP proxy written in Go, specifically designed to optimize Video On Demand (VOD) streaming (such as IPTV streams, large video assets, and static media files). 

It dynamically proxies video requests, downloads and caches content on-demand using a high-precision **blockless continuous byte-range engine**, handles byte-range seeking requests with sub-20ms TTFB, and implements automated cache eviction, container metadata prefetching, and upstream revalidation.

---

## 🚀 Key Features

- **On-Demand Caching & Non-Blocking Commits**: Media files are cached on-demand when requested by a client. Once triggered, the proxy downloads the media in parallel to guarantee a complete local copy. Upon completion, file finalization (`.part` to final cache filename) occurs instantly via non-blocking atomic rename (<1ms) without hardware storage barriers (`fsync`), ensuring concurrent downloads run smoothly with zero freezes. Upstream CDN bandwidth is saved by deduplicating concurrent and subsequent requests for the same media file.
- **Continuous Byte-Range Streaming & Sub-20ms TTFB**: Unlike legacy chunked proxies that force players to wait seconds for full 4MB blocks to buffer, streamcache streams upstream data to disk in 128KB network slices with instant client wakeups. Clients begin playing within milliseconds of the first byte arriving.
- **Multi-Worker State Synchronization & Starvation Scheduling**: The proxy coordinates media downloads across multiple parallel workers (configured via `MAX_UPSTREAM_WORKERS`). If a player seeks to a different part of the video, the coordinator instantly shifts priority by spawning or pre-empting download workers to satisfy the starved offset first:
  - *Collision Leapfrogging & Gap Backfill*: Worker threads that reach already-cached data automatically leapfrog over completed intervals to the next un-downloaded gap. When downstream ranges finish, workers automatically backfill earlier skipped gaps until the media file is 100% complete.
  - *Client Hydration Wait*: If an active worker is already streaming toward a starved offset and its estimated time of arrival (ETA) is under `CLIENT_HYDRATION_WAIT` (default `2s`), the proxy suppresses spawning new connections, conserving valuable upstream network sessions.
  - *Starvation Stall Breakout*: If all workers actively covering a starved offset have been throttled without incoming download activity for $\ge$ `STARVATION_STALL_TIMEOUT` (default `30s`), the coordinator cleanly breaks out of proximity suppression, aborts and reaps the stalled worker socket(s) to free up concurrency slots, and reconnects directly at the client's starving offset. When worker concurrency limits are reached during seeks or jumps, stalled workers are also prioritized for preemption first. `SCHEDULER_MIN_TIME` (default `2s`) acts as a startup grace period on each reconnect to prevent connection churn on bursty providers.
- **Exact Container Metadata Pre-fetching**: As soon as media download begins and the initial 128KB container header is buffered to disk, the proxy automatically inspects MP4 (`moov`) and MKV (`Cues`) headers. If a non-faststart MP4 (where `mdat` precedes `moov`) or MKV with a tail index is detected, the proxy calculates the exact byte offset and immediately spawns a parallel prefetch helper worker (or triggers an inline tail fetch in single-worker mode) to download the trailing metadata range. Player index probes are satisfied instantly from local disk, eliminating player startup stalls, connection thrashing, and seek timeouts.
- **Full Range Request Support**: Fully compatible with standard HTTP `Range` and `Accept-Ranges` headers. Video players can seek forward or backward smoothly.
- **Smart Cache Pre-Eviction (LRU)**: Automatically pre-evicts inactive cached files in LRU order at the start of hydration as soon as the media file size is determined, ensuring sufficient disk space and cache quota before downloads begin.
- **Incomplete Download Garbage Collection**: If a background download is aborted or fails during runtime, the proxy automatically cleans up the incomplete `.part` file, deletes the companion JSON metadata, and clears the file from memory as soon as all clients disconnect, reclaiming disk space instantly.
- **Asynchronous Pipelined Writeback & Non-Blocking Commits**:
  - *Decoupled Storage Writeback*: Ingests streaming upstream network chunks into a bounded producer-consumer queue (`WRITEBACK_BUFFER_SIZE`, default `64MB`), decoupling network downloads from disk I/O bottlenecks.
  - *Zero-Copy LiveBuffer Fast-Path*: Active video players reading near the live edge of active hydrations stream directly from RAM (`LiveBuffer`), achieving sub-0.1ms TTFB without disk read operations.
  - *Native Continuous Linux Writeback Pacing*: On Linux, paces hardware writeouts asynchronously via `unix.SyncFileRange(SYNC_FILE_RANGE_WRITE)` in 8MB windows, followed by `unix.SyncFileRange(WAIT_BEFORE|WRITE|WAIT_AFTER)` and `unix.Fadvise(POSIX_FADV_DONTNEED)` to evict clean pages immediately from host RAM. Prevents multi-gigabyte page cache saturation and container OOM kills.
  - *Asynchronous Finalization & Commit*: When a download completes, the coordinator immediately hands off commit finalization to a background worker (`finalizeAndCommitCache`), keeping `Committing` status visible on the dashboard without blocking other active hydrations or dropping network download speeds.
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
- **SSE Web Dashboard & Telemetry Visualizers**: Includes a high-performance web dashboard powered by a unified, authoritative SSE stream (dynamic 250ms cadence during active transfers/playback, scaling down to 3s when idle). Features include:
  - *Dual-Color Playback Needles & Canvas Overlays*: Real-time canvas overlay rendering continuous green download chunks, orange starving hashed regions, and client playback position needles (**Neon Cyan** for normal/buffered playback, **Alert Red** for starved playback) with interactive hover tooltips.
  - *Peak-Scaled Speed Gradient & Stall Halo*: Live download speed card with background fill scaled from 0 to peak observed throughput (`MaxObservedSpeed`), plus an animated pulsating orange halo when transfers stall for $\ge 3\text{s}$.
  - *Collapsible Active Client Sessions*: Compact collapsible panel tracking active player sessions, offsets, percentages, and playback health without dashboard clutter.

---

## ⚙️ Configuration Guide

streamcache is configured using environment variables. Default fallback values are designed for local development.

| Environment Variable | Default | Description |
|---|---|---|
| `PORT` | `:8080` | Port the proxy server listens on. |
| `CACHE_DIR` | `./cache/files` | Location where completed cached media files and JSON metadata are stored. |
| `TMP_DIR` | `./cache/tmp` | Location where active `.part` downloads are assembled. |
| `CACHE_MAX_BYTES` | `50GB` | Maximum size of the cache directory. Set to `0`, `none`, or `off` to disable the limit and rely solely on disk space checks. Supports size units (e.g. `50GB`, `1TB`). |
| `MIN_DISK_FREE_BYTES` | `10GB` | Safety buffer for physical disk space that overrides the cache size limit to prevent disk filling. Supports size units (e.g. `10GB`). |
| `CACHE_REVALIDATE_INTERVAL` | `1h` | Cache expiration age before forcing an HTTP revalidation probe to the source host. |
| `ALLOWED_HOSTS` | *(empty)* | Optional comma-separated list of domain names (e.g. `cdn.example.com`) to restrict which upstreams can be proxied. |
| `SCHEDULER_MIN_TIME` | `2s` | Minimum duration to stream a segment before switching worker targets (debouncer against seek thrashing and startup grace period). Set to `0` or `none` to disable. |
| `STARVATION_STALL_TIMEOUT` | `30s` | Duration a downloading worker must be throttled without download activity before a starving client triggers the coordinator to abort the stalled worker and reconnect directly at the starving offset. |
| `UPSTREAM_READ_TIMEOUT` | `0s` (unlimited) | Read timeout for stalled upstream reads. Default is `0s` (unlimited/no timeout), allowing IPTV providers with pause/burst throttling to remain connected. Can be set to a duration (e.g. `30s`, `90s`) to tune down if needed. |
| `MAX_UPSTREAM_RETRIES` | `0` (unlimited) | Maximum consecutive retry attempts per worker before worker self-termination. Default is `0` (unlimited retries). Can be set to a positive integer (e.g. `5`, `8`) to cap retries. |
| `MAX_UPSTREAM_WORKERS` | `2` | Maximum concurrent upstream downloader threads per VOD file. |
| `CLIENT_HYDRATION_WAIT` | `2s` | Duration the proxy waits for active hydration to catch up sequentially before spawning a new connection (Jump Suppression). |
| `PROBE_FAILURE_COOLDOWN` | `10m` | Cooldown duration for caching catastrophic/permanent probe failures (e.g. HTML blocks, HTTP 403/404) to block retries. |
| `PROBE_TRANSIENT_COOLDOWN` | `30s` | Cooldown duration for caching transient probe failures (e.g. timeouts, network drops, HTTP 500/503) to absorb retry storms. |
| `UPSTREAM_USER_AGENT_MODE` | `smart` | User-Agent spoofing mode for upstream CDN requests (`smart`, `force`, `passthrough`). `smart` passes valid player UAs while automatically rewriting throttled/bot signatures (e.g. `Lavf`, `FFmpeg`, `Unknown`). `force` always overrides all client UAs. `passthrough` passes client UAs, using default only if empty. |
| `UPSTREAM_USER_AGENT` | `VLC/3.0.20 LibVLC/3.0.20` | Upstream User-Agent string sent to CDNs when spoofing or fallback is applied. |
| `WRITEBACK_BUFFER_SIZE` | `64MB` | Size of the in-memory writeback ring buffer and LiveBuffer sliding window (bounds: `16MB` to `256MB`). Decouples upstream network ingestion from storage I/O, speeds up live-edge playback directly from RAM, and paces asynchronous kernel writebacks without stalling concurrent downloads. |
| `LOG_VERBOSE` | `false` | Enable verbose per-packet scheduler trace logging for debugging. When `false`, suppresses routine in-flight check noise. |
| `DISPATCHARR_URL` | *(empty)* | Base URL of Dispatcharr (e.g. `http://dispatcharr:9191`). Enables real-time capacity polling via Stasharr (`/plugins/stasharr/provider-capacity`). |
| `PREHYDRATE_CAPACITY_RESERVATION` | `1` | Number of free provider stream slots to reserve strictly for live human viewers. Background prehydration will not start unless `available >= reservation + 1`, and will yield if `available < reservation`. *(Alias: `PREFETCH_CAPACITY_RESERVATION`)* |
| `PREHYDRATE_YIELD_BACKOFF` | `30s` | Stabilization cooldown duration applied when prehydration yields to protect providers from rapid socket churn. *(Alias: `PREFETCH_YIELD_BACKOFF`)* |
| `EVICT_YIELDED_PARTS` | `false` | Whether to delete `.part` files when a prehydration task yields. When `false` (default), partial downloads are preserved so prehydration resumes right where it left off. |
| `ENABLE_EPISODE_PREHYDRATION` | `true` | Master toggle for background sequential episode prehydration via `&next_url=`. *(Alias: `ENABLE_EPISODE_PREFETCH`)* |

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
   docker compose up -d --build
   ```
2. Monitor application logs:
   ```bash
   docker compose logs -f
   ```
3. Stop the container:
   ```bash
   docker compose down
   ```

*Note: Docker Compose pulls and builds directly from the public GitHub repository (`https://github.com/bruor/streamcache.git#main`). If you prefer to build from your local repository files instead, simply change `context:` to `.` in `docker-compose.yml`.*

---

## 📺 How to Use the Proxy

Once streamcache is running, you can stream media by pointing your video players (e.g. VLC, MPV, Kodi, TiviMate, or HTML5 player) to the proxy using either of the routing methods:

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

- **SSE Web Dashboard**: `http://localhost:8080/` (displays cache storage status, active downloads with continuous range progress bars, system logs, and stable client playback sessions with play offset, percentage progress, active connection count, and downloaded/hydrating/idle status tracking)
- **JSON Registry State**: `http://localhost:8080/debug/state` (exposes memory maps of cached byte ranges and detailed client connection metadata)
- **Go pprof Profile Endpoint**: `http://localhost:8080/debug/pprof/` (runtime profiling tools)
