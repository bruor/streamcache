# **Disclaimer**
This solution was created using copilot via prompt engineering and bug fixing.  It leverages nginx and lua via a docker container, its goal is to allow a player to request a stream source which kicks off a background process to download the resulting file into configurable cache on disk.  Open to refinements from the community!
Cache maintenance functionality is untested, I haven't yet tested whether or not the worker threads properly manage the cache size yet. 

## **Functional Overview**
Map whatever port number you want into the container, it will listen for incoming requests in the following format: 
```
http://<hostname_or_ip>:<port>/u/<base64_encoded_url>
```

When queried a background process will kick off a download on the media file hosted at the encoded URL and tee the response to the player, subsequent responses to the player will be served from the cache for the in-progress or completed download.  This allows emby/jellyfin to start a .strm file a bit faster than it normally can, and helps keep your provider from seeing requests for the same content twice every time you play something.  Furthermore, some providers use an API for content and will return different files when the URL is called back to back which causes emby remux/transcodes to fail. 
The back-end process follows redirects to the content.  If an HTTP error occurs, HTML is output to give you a little information about why, this could use some enhancement, a playable video showing the upstream error would be a nice touch.  I've also thought about having an error (or even a request) generate a download request in jellyseer so that provider content issues can be automatically worked around, maybe one day. 

Every CACHE_JANITOR_INTERVAL, the system will check the size of the cache, if it is above CACHE_MAX_BYTES it will evict files that have the oldest last-accessed timestamps until the cache size drops below CACHE_LOW_WATERMARK_BYTES.


# **Deployment and Config**

## **Docker folder setup**

### **Dockerfile**
```
# ~/streamcache/Dockerfile
FROM openresty/openresty:alpine

# OPM needs perl and curl; CA certs for HTTPS
RUN apk add --no-cache perl curl ca-certificates && update-ca-certificates

# Install lua-resty-http via OPM
RUN /usr/local/openresty/bin/opm get ledgetech/lua-resty-http
```

### **docker-compose.yml**
```
# ~/streamcache/docker-compose.yml
services:
  streamcache:
    build: .
    image: streamcache:latest
    container_name: streamcache
    ports:
      - "8080:8080"
    restart: unless-stopped
    environment:
      CACHE_MAX_BYTES: "536870912000"        # 500 GB
      CACHE_LOW_WATERMARK_BYTES: "483183820800"  # 450 GB
      CACHE_JANITOR_INTERVAL: "300" #5mins
      ALLOWED_HOSTS: ""
      DISABLE_SSL_VERIFY: "0"
      LOG_VERBOSE: "1"                       # <-- set to "0" to reduce log noise
    volumes:
      - ./conf/nginx.conf:/usr/local/openresty/nginx/conf/nginx.conf:ro
      - ./conf/streamcache.conf:/etc/openresty/conf.d/streamcache.conf:ro
      #- ./cache:/var/cache/streamcache
      - /mnt/data/streamcache:/var/cache/streamcache
    command: ["/usr/local/openresty/bin/openresty", "-g", "daemon off;"]
```

### **conf/nginx.conf**
```
# ~/streamcache/conf/nginx.conf

# ---------- ENV (main context only) ----------
env CACHE_MAX_BYTES;
env CACHE_LOW_WATERMARK_BYTES;
env CACHE_JANITOR_INTERVAL;
env ALLOWED_HOSTS;
env DISABLE_SSL_VERIFY;
env LOG_VERBOSE;
env RESOLVE_TTL;
# --------------------------------------------

worker_processes auto;

events {
    worker_connections 2048;
}

http {
    include       mime.types;
    default_type  application/octet-stream;

    sendfile        on;
    tcp_nopush      on;
    tcp_nodelay     on;
    keepalive_timeout  65;

    # Lua module paths (for resty.http)
    lua_package_path "/usr/local/openresty/site/lualib/?.lua;/usr/local/openresty/lualib/?.lua;;";
    lua_package_cpath "/usr/local/openresty/site/lualib/?.so;/usr/local/openresty/lualib/?.so;;";

    # DNS for upstreams
    resolver 8.8.8.8 1.1.1.1 ipv6=off valid=300s;

    # Let Lua verify HTTPS using the system CA bundle
    lua_ssl_trusted_certificate /etc/ssl/certs/ca-certificates.crt;
    lua_ssl_verify_depth 5;

    # Logs to Docker
    log_format main '$remote_addr - $remote_user [$time_local] "$request" '
                    '$status $body_bytes_sent "$http_referer" '
                    '"$http_user_agent" "$http_range" '
                    'rt=$request_time up_rt=$upstream_response_time';
    access_log /dev/stdout main;
    error_log  /dev/stderr info;

    # Shared dicts
    lua_shared_dict inflight 10m;   # prevent duplicate full fetches
    lua_shared_dict access  50m;    # last-access timestamps (LRU)
    lua_shared_dict resolved 10m;   # cache of resolved CDN URLs (per key)

    client_max_body_size 0;

    # ----- Human-readable logging + janitor registration -----
    init_worker_by_lua_block {
        local ngx = ngx
        local access = ngx.shared.access

        local CACHE_DIR = "/var/cache/streamcache/files"
        local META_DIR  = "/var/cache/streamcache/meta"

        local function b2human(n)
            if not n or n < 0 then return "0 B" end
            local units = {"B","KB","MB","GB","TB","PB"}
            local i = 1
            while n >= 1024 and i < #units do n = n / 1024; i = i + 1 end
            return string.format("%.2f %s", n, units[i])
        end

        local VERBOSE = (os.getenv("LOG_VERBOSE") == "1")

        local function file_size(path)
            local f = io.open(path, "rb"); if not f then return 0 end
            local sz = f:seek("end"); f:close(); return sz or 0
        end

        local function parse_meta(path)
            local f = io.open(path, "rb"); if not f then return nil end
            local size, last = nil, nil
            for line in f:lines() do
                local k, v = line:match("^([a-z_]+)=(.+)$")
                if k == "size" then size = tonumber(v)
                elseif k == "last_access" then last = tonumber(v)
                end
            end
            f:close()
            return size, last
        end

        local function janitor(premature)
            if premature then return end
            local MAX = tonumber(os.getenv("CACHE_MAX_BYTES") or "") or (500 * 1024^3)
            local LOW = tonumber(os.getenv("CACHE_LOW_WATERMARK_BYTES") or "") or (450 * 1024^3)
            local t0 = ngx.now()

            os.execute("mkdir -p " .. CACHE_DIR .. " " .. META_DIR)

            local files, total = {}, 0
            local p = io.popen("ls -1 " .. CACHE_DIR .. " 2>/dev/null")
            if p then
                for name in p:lines() do
                    local key = name
                    local path = CACHE_DIR .. "/" .. name
                    local meta_path = META_DIR .. "/" .. key .. ".meta"
                    local size, last = parse_meta(meta_path)
                    if not size then size = file_size(path) end
                    if not last then last = access:get(key) or 0 end
                    files[#files+1] = {key=key, path=path, meta=meta_path, size=size, last=last}
                    total = total + (size or 0)
                end
                p:close()
            end

            if VERBOSE then
                ngx.log(ngx.INFO, string.format(
                    "[streamcache] janitor check: total=%s (%d bytes), max=%s, low=%s, files=%d",
                    b2human(total), total, b2human(MAX), b2human(LOW), #files))
            else
                ngx.log(ngx.NOTICE, string.format(
                    "[streamcache] janitor: total=%s, max=%s, low=%s",
                    b2human(total), b2human(MAX), b2human(LOW)))
            end

            if total <= MAX then return end

            table.sort(files, function(a,b) return (a.last or 0) < (b.last or 0) end)

            local removed_files, removed_bytes = 0, 0
            for _, f in ipairs(files) do
                if total <= LOW then break end
                os.remove(f.path)
                os.remove(f.meta)
                access:delete(f.key)
                total = total - (f.size or 0)
                removed_files = removed_files + 1
                removed_bytes = removed_bytes + (f.size or 0)
                if VERBOSE then
                    ngx.log(ngx.INFO, string.format(
                        "[streamcache] evict: key=%s size=%s", f.key, b2human(f.size or 0)))
                end
            end

            ngx.log(ngx.NOTICE, string.format(
                "[streamcache] eviction done in %.2fs: removed %d (%s), new total=%s",
                ngx.now() - t0, removed_files, b2human(removed_bytes), b2human(total)))
        end

        local MAX = tonumber(os.getenv("CACHE_MAX_BYTES") or "") or (500 * 1024^3)
        local LOW = tonumber(os.getenv("CACHE_LOW_WATERMARK_BYTES") or "") or (450 * 1024^3)
        local interval = tonumber(os.getenv("CACHE_JANITOR_INTERVAL") or "") or 60

        if ngx.worker.id() == 0 then
          ngx.log(ngx.NOTICE, string.format(
              "[streamcache] start: max=%s (%d), low=%s (%d), interval=%ds, verbose=%s",
              b2human(MAX), MAX, b2human(LOW), LOW, interval, tostring(VERBOSE)))

          local ok, err = ngx.timer.every(interval, janitor)
          if not ok then ngx.log(ngx.ERR, "janitor timer failed: ", err) end
        else
          if VERBOSE then
              ngx.log(ngx.INFO, "[streamcache] worker " .. ngx.worker.id() .. " not leader; janitor disabled")
          end
        end

    }

    include /etc/openresty/conf.d/*.conf;
}
```

### **conf/streamcache.conf**
```
server {
    listen 8080;
    server_name _;

    # Carry decoded upstream URL to proxy_pass when needed
    set $upstream_url "";

    # Serve cached files directly; Nginx static supports Range natively
    location /cache/ {
        internal;
        alias /var/cache/streamcache/files/;
        add_header Accept-Ranges bytes always;
    }

    # Main endpoint: /u/<BASE64URL-ENCODED-ORIGINAL-URL>
    location ~ ^/u/(?<b64>[-_A-Za-z0-9=]+)$ {
        content_by_lua_block {
            local ngx = ngx
            local os  = os
            local http = require "resty.http"

            -- ===== Helpers =====
            local VERBOSE = (os.getenv("LOG_VERBOSE") == "1")
            local RESOLVE_TTL = tonumber(os.getenv("RESOLVE_TTL") or "") or 60

            local function b2human(n)
                if not n or n < 0 then return "0 B" end
                local units = {"B","KB","MB","GB","TB","PB"}
                local i = 1
                while n >= 1024 and i < #units do n = n / 1024; i = i + 1 end
                return string.format("%.2f %s", n, units[i])
            end

            local function write_meta(key, size, last)
                local meta_dir = "/var/cache/streamcache/meta"
                os.execute("mkdir -p " .. meta_dir)
                local f = io.open(meta_dir .. "/" .. key .. ".meta", "wb")
                if not f then return end
                f:write("size=" .. tostring(size or 0) .. "\n")
                f:write("last_access=" .. tostring(last or ngx.now()) .. "\n")
                f:close()
            end

            local function b64url_decode(input)
                input = input:gsub('-', '+'):gsub('_', '/')
                local pad = #input % 4
                if pad > 0 then input = input .. string.rep('=', 4 - pad) end
                return ngx.decode_base64(input)
            end

            local function file_exists(p) local f=io.open(p,"rb"); if f then f:close(); return true end return false end
            local function file_size(p) local f=io.open(p,"rb"); if not f then return 0 end local sz=f:seek("end"); f:close(); return sz or 0 end
            local function key_from_url(u) return ngx.md5(u) end

            -- split "authority" into host + port (supports IPv6)
            local function split_hostport(authority, scheme)
                if not authority or authority == "" then return nil, nil end
                -- IPv6 literal
                local h, p = authority:match("^%[([^%]]+)%]:(%d+)$")
                if h then return h, tonumber(p) end
                h = authority:match("^%[([^%]]+)%]$"); if h then
                    return h, (scheme == "https" and 443 or 80)
                end
                -- host:port
                h, p = authority:match("^([^:]+):(%d+)$")
                if h then return h, tonumber(p) end
                -- host only
                return authority, (scheme == "https" and 443 or 80)
            end

            -- robust URL parser: returns scheme, host, port, path, host_header
            local function parse_url(u)
                if not u then return nil end
                local scheme, rest = u:match("^(https?)://(.+)$")
                if not scheme then return nil end
                local authority, path = rest:match("^([^/]+)(/.*)$")
                authority = authority or rest
                path = path or "/"
                local host, port = split_hostport(authority, scheme)
                if not host then return nil end
                local default = (scheme == "https" and 443 or 80)
                port = port or default
                local host_header = (port == default) and host or (host .. ":" .. tostring(port))
                return scheme, host, port, path, host_header
            end

            -- normalize Location into absolute URL
            local function resolve_location(current, scheme, host, loc)
                if not loc or loc == "" then return nil end
                -- absolute
                if loc:match("^https?://") then return loc end
                -- scheme-relative: //host[:port]/path
                if loc:match("^//") then return scheme .. ":" .. loc end
                -- authority-only: host[:port]/path
                if loc:match("^[^/]+:%d+/.") or loc:match("^[^/]+/") then
                    return scheme .. "://" .. loc
                end
                -- root-relative
                if loc:sub(1,1) == "/" then
                    return scheme .. "://" .. host .. loc
                end
                -- relative (append to current dir)
                local base = current:match("^(https?://[^?]+)")
                local dir = base and base:match("(.*/)") or (scheme .. "://" .. host .. "/")
                return dir .. loc
            end

            -- Follow redirects (HEAD then GET Range:0- fallback) -> final absolute URL
            local function follow_redirects(url, max_hops)
                local ssl_verify = (os.getenv("DISABLE_SSL_VERIFY") ~= "1")
                local current = (url or ""):gsub("^%s+", ""):gsub("[%s\r\n]+$", "")
                for _=1,(max_hops or 5) do
                    local httpc = http.new()
                    httpc:set_timeouts(3000, 3000, 3000)

                    -- parse
                    local scheme, host, port, path, host_header
                    do
                        local sch, rest = current:match("^(https?)://(.+)$")
                        if not sch then return nil, "bad_url" end
                        scheme = sch
                        local authority, pth = rest:match("^([^/]+)(/.*)$")
                        authority = authority or rest
                        path = pth or "/"
                        host, port = split_hostport(authority, scheme)
                        if not host then return nil, "bad_url" end
                        local def = (scheme == "https" and 443 or 80)
                        port = port or def
                        host_header = (port == def) and host or (host .. ":" .. tostring(port))
                    end

                    local ok, err = httpc:connect(host, port)
                    if not ok then httpc:close(); return nil, "connect:"..tostring(err) end
                    if scheme == "https" then
                        local ok2, err2 = httpc:ssl_handshake(nil, host, ssl_verify)
                        if not ok2 then httpc:close(); return nil, "tls:"..tostring(err2) end
                    end

                    local headers = { ["Host"] = host_header, ["User-Agent"] = "EmbyStreamCache/1.0", ["Referer"] = url }
                    local res, rerr = httpc:request{ method = "HEAD", path = path, headers = headers }
                    if not res or res.status == 405 then
                        headers["Range"] = "bytes=0-0"
                        res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
                    end
                    if not res then httpc:close(); return nil, "request:"..tostring(rerr) end

                    if res.status == 301 or res.status == 302 or res.status == 303 or res.status == 307 or res.status == 308 then
                        local loc = res.headers and (res.headers["Location"] or res.headers["location"])
                        httpc:close()
                        local next_url = resolve_location(current, scheme, host_header, loc)
                        if not next_url then return nil, "bad_redirect" end
                        current = next_url
                    elseif res.status >= 200 and res.status < 300 then
                        httpc:close()
                        return current, nil
                    else
                        httpc:close()
                        return nil, "status_" .. tostring(res.status)
                    end
                end
                return nil, "too_many_redirects"
            end

            -- Tee: stream upstream -> client and -> file (for no-Range or Range: bytes=0-)
            local function tee_stream(final_url, dest_path, key, use_range_0)
                final_url = (final_url or ""):gsub("^%s+", ""):gsub("[%s\r\n]+$", "")
                local scheme, host, port, path, host_header = parse_url(final_url)
                if not scheme then
                    ngx.log(ngx.WARN, "[streamcache] tee parse failed url=", tostring(final_url))
                    return ngx.exit(ngx.HTTP_BAD_GATEWAY)
                end
                local ssl_verify = (os.getenv("DISABLE_SSL_VERIFY") ~= "1")
                local httpc = http.new()
                httpc:set_timeouts(5000, 5000, 0)

                local ok, err = httpc:connect(host, port)
                if not ok then ngx.log(ngx.WARN, "[streamcache] tee connect failed: ", tostring(err)); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
                if scheme == "https" then
                    local ok2, err2 = httpc:ssl_handshake(nil, host, ssl_verify)
                    if not ok2 then ngx.log(ngx.WARN, "[streamcache] tee TLS failed: ", tostring(err2)); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
                end

                local headers = { ["Host"] = host_header, ["User-Agent"] = "EmbyStreamCache/1.0", ["Referer"] = ngx.var.scheme .. "://" .. ngx.var.host }
                if use_range_0 then headers["Range"] = "bytes=0-" end

                local res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
                if not res then ngx.log(ngx.WARN, "[streamcache] tee request failed: ", tostring(rerr)); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
                if not (res.status == 200 or res.status == 206) then
                    ngx.log(ngx.WARN, "[streamcache] tee status not OK: ", res.status)
                    httpc:close()
                    return ngx.exit(ngx.HTTP_BAD_GATEWAY)
                end

                -- Prepare response headers to client
                local hcopy = {}
                for k,v in pairs(res.headers or {}) do
                    local kl = string.lower(k)
                    if kl ~= "transfer-encoding" and kl ~= "connection" and kl ~= "keep-alive"
                       and kl ~= "proxy-authenticate" and kl ~= "proxy-authorization"
                       and kl ~= "te" and kl ~= "trailer" and kl ~= "upgrade" then
                        hcopy[k] = v
                    end
                end
                hcopy["Accept-Ranges"] = "bytes"
                ngx.status = res.status
                for k,v in pairs(hcopy) do ngx.header[k] = v end
                ngx.send_headers()

                -- Open temp file
                os.execute("mkdir -p /var/cache/streamcache/tmp")
                local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"
                local f = io.open(tmp, "wb")
                if not f then ngx.log(ngx.ERR, "[streamcache] tee cannot open temp: ", tmp); httpc:close(); return ngx.exit(ngx.HTTP_INTERNAL_SERVER_ERROR) end

                local total = 0
                local client_alive = true

                if res.body_reader then
                    while true do
                        local chunk, cerr = res.body_reader(8192)
                        if cerr then
                            ngx.log(ngx.WARN, "[streamcache] tee read error: ", tostring(cerr))
                            f:close(); os.remove(tmp); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY)
                        end
                        if not chunk then break end
                        total = total + #chunk
                        f:write(chunk)
                        if client_alive then
                            local okp = pcall(ngx.print, chunk)
                            if not okp then client_alive = false else ngx.flush(true) end
                        end
                    end
                elseif res.body then
                    total = #res.body
                    f:write(res.body)
                    if client_alive then pcall(ngx.print, res.body); ngx.flush(true) end
                end

                f:flush(); f:close()
                httpc:close()

                if total > 0 then
                    os.rename(tmp, dest_path)
                    local ts = ngx.now()
                    write_meta(key, total, ts)
                    ngx.shared.access:set(key, ts)
                    if VERBOSE then ngx.log(ngx.NOTICE, "[streamcache] tee complete key=", key, " size=", b2human(total)) end
                else
                    os.remove(tmp)
                    ngx.log(ngx.WARN, "[streamcache] tee produced 0 bytes; not committing key=", key)
                end
                return
            end
            -- =====================

            local inflight = ngx.shared.inflight
            local access   = ngx.shared.access
            local resolved = ngx.shared.resolved

            -- Decode Base64URL
            local b64  = ngx.var.b64
            local orig = b64 and b64url_decode(b64) or nil
            if not orig or not orig:match("^https?://") then
                ngx.log(ngx.WARN, "[streamcache] bad request (invalid or non-http URL)")
                return ngx.exit(ngx.HTTP_BAD_REQUEST)
            end
            orig = orig:gsub("^%s+", ""):gsub("[%s\r\n]+$", "")

            -- Optional SSRF protection
            local allowed = os.getenv("ALLOWED_HOSTS")
            if allowed and allowed ~= "" then
                local host = orig:match("^https?://([^/]+)")
                local ok = false
                for h in allowed:gmatch("[^,%s]+") do if host == h then ok = true; break end end
                if not ok then
                    ngx.log(ngx.WARN, "[streamcache] blocked host: ", host)
                    ngx.status = ngx.HTTP_FORBIDDEN
                    ngx.say("Host not allowed")
                    return
                end
            end

            local files_dir = "/var/cache/streamcache/files"
            os.execute("mkdir -p " .. files_dir .. " /var/cache/streamcache/tmp /var/cache/streamcache/meta")

            local key = key_from_url(orig)
            local final_path = files_dir .. "/" .. key

            local function update_last_access()
                local ts = ngx.now()
                access:set(key, ts)
                write_meta(key, file_size(final_path), ts)
            end

            -- HIT (with zero-byte guard)
            if file_exists(final_path) then
                local sz = file_size(final_path)
                if sz <= 0 then
                    ngx.log(ngx.WARN, "[streamcache] zero-byte cached file; purging key=", key)
                    os.remove(final_path)
                    os.remove("/var/cache/streamcache/meta/" .. key .. ".meta")
                    inflight:delete(key)
                else
                    ngx.header["X-Cache"] = "HIT"
                    if VERBOSE then ngx.log(ngx.INFO, "[streamcache] HIT key=", key, " size=", b2human(sz)) end
                    update_last_access()
                    return ngx.exec("/cache/" .. key)
                end
            end

            -- Resolve final CDN URL (cached briefly)
            local function get_final_url_for_key()
                local cached = resolved:get(key)
                if cached and cached ~= "" then return cached end
                local fu, ferr = follow_redirects(orig, 5)
                if not fu then
                    ngx.log(ngx.WARN, "get_final_url_for_key(): [streamcache] redirect resolution failed: ", tostring(ferr))
                    ngx.status = ngx.HTTP_BAD_GATEWAY
                    ngx.header["Content-Type"] = "text/plain"
                    ngx.say("Upstream auth redirect failed; see proxy logs.")
                    return nil
                end
                resolved:set(key, fu, RESOLVE_TTL)
                return fu
            end

            local final_url = get_final_url_for_key()
            if not final_url then return end  -- 502 already returned

            -- Determine if we can tee this request
            local req_range = ngx.req.get_headers()["Range"]
            local tee_ok, use_range_0 = false, false
            if not req_range or req_range == "" then
                tee_ok, use_range_0 = true, false
            else
                local s, e = req_range:match("^bytes=(%d+)%-(%d*)$")
                if s and tonumber(s) == 0 and (not e or e == "") then
                    tee_ok, use_range_0 = true, true
                end
            end

            if tee_ok then
                local added = ngx.shared.inflight:add(key, true, 900)
                if not added then
                    -- Another tee in progress â€” proxy (no cache) but keep client on proxy
                    ngx.header["X-Cache"] = "MISS"
                    ngx.var.upstream_url = final_url
                    if VERBOSE then ngx.log(ngx.INFO, "[streamcache] tee busy; simple proxy key=", key) end
                    return ngx.exec("@passthrough")
                end
                -- Single connection: upstream -> client & file
                return tee_stream(final_url, final_path, key, use_range_0)
            else
                -- Non-teeable range: strictly proxy the resolved final URL
                ngx.header["X-Cache"] = "MISS"
                ngx.var.upstream_url = final_url
                if VERBOSE then ngx.log(ngx.INFO, "[streamcache] non-tee range; proxy only key=", key, " range=", tostring(req_range)) end
                return ngx.exec("@passthrough")
            end
        }
    }

    # Strict proxy for non-tee paths or when tee is busy
    location @passthrough {
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $proxy_host;
        proxy_set_header User-Agent "EmbyStreamCache/1.0";
        proxy_intercept_errors off;

        # Forward Range header to origin
        proxy_set_header Range $http_range;

        # SNI for HTTPS variable upstreams
        proxy_ssl_server_name on;
        proxy_ssl_name $proxy_host;
        proxy_ssl_protocols TLSv1.2 TLSv1.3;

        proxy_pass_request_headers on;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;

        proxy_pass $upstream_url;
    }
}
```

## **Build the docker and start it **
```
docker compose build --no-cache
docker compose up -d 
```
