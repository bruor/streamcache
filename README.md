# **Disclaimer**
This solution was created using copilot via prompt engineering and bug fixing.  It leverages nginx and lua via a docker container, its goal is to allow a player to request a stream which transparently kicks off a background process to download the resulting file into configurable cache on disk.  Open to enhancements from the community!

## **Functional Overview**
Map whatever port number you want into the container, it will listen for incoming requests in the following format: 
```
http://<hostname_or_ip>:<port>/u/<base64_encoded_url>
```

When queried a background process will kick off a download on the media file hosted at the encoded URL and tee the response to the player, subsequent responses to the player will be served from the cache for the in-progress or completed download.  This allows emby/jellyfin to start a .strm file a bit faster than it normally can, and helps keep your provider from seeing requests for the same content twice every time you play something.  Furthermore, some providers use an API for content and will return different files when the URL is called back to back which causes emby remux/transcodes to fail. 
The back-end process follows redirects to the content.  If an HTTP error occurs, HTML is output to give you a little information about why, this could use some enhancement, a playable video showing the upstream error would be a nice touch.  I've also thought about having an error (or even a request) generate a download request in jellyseer so that provider content issues can be automatically worked around, maybe one day. 

Every CACHE_JANITOR_INTERVAL, the system will check the size of the cache, if it is above CACHE_MAX_BYTES it will evict files that have the oldest last-accessed timestamps until the cache size drops below CACHE_LOW_WATERMARK_BYTES.

To control how tee behavior works, use these environment variables in your docker-compose.yml
RANGE_TEE_THRESHOLD: "1048576"   # 1 MiB near-start window, if an initial request is within this window, start a full download and service client request from in-flight cache
PROGRESS_FLUSH_BYTES: "262144"   # writer flush/report cadence, when this much data is received from the upstream server, flush it to the file on disk
FOLLOWER_WAIT_MAX: "60"          # seconds the follower will wait for bytes, maximum time that the proxy will wait for a range request to be fulfilled before it is abandoned.
FOLLOWER_POLL_MS: "50"           # follower poll interval (ms), how often follower thread polls the disk cache for new data to respond to range requests. 

# **Deployment and Config**

## **Docker folder setup**
```Docker/
├── Dockerfile
├── docker-compose.yml
└── conf/
    ├── nginx.conf
    ├── streamcache.conf
    ├── streamcache.lua
```


## **Build the docker and start it **
```
docker compose build --no-cache
docker compose up -d 
```
