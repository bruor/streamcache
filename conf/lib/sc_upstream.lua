-- sc_upstream.lua
-- HTTP client with centralized redirect following.
-- All upstream communication goes through this module for consistent
-- header handling and detection-avoidance behavior.

local http  = require "resty.http"
local ngx   = ngx
local Utils = require "sc_utils"

local Upstream = {}

-- ======================== URL Parsing ========================

function Upstream.split_hostport(authority, scheme)
  if not authority or authority == "" then return nil, nil end
  -- IPv6 with port: [::1]:8080
  local h, p = authority:match("^%[(.-)%]:(%d+)$")
  if h then return h, tonumber(p) end
  -- IPv6 without port: [::1]
  h = authority:match("^%[(.-)%]$")
  if h then return h, (scheme == "https" and 443 or 80) end
  -- IPv4/hostname with port
  h, p = authority:match("^([^:]+):(%d+)$")
  if h then return h, tonumber(p) end
  -- Bare hostname
  return authority, (scheme == "https" and 443 or 80)
end

function Upstream.parse_url(u)
  if not u then return nil end
  local scheme, rest = u:match("^(https?)://(.+)$")
  if not scheme then return nil end
  local authority, path = rest:match("^([^/]+)(/.*)$")
  authority = authority or rest
  path = path or "/"
  local host, port = Upstream.split_hostport(authority, scheme)
  if not host then return nil end
  local def = (scheme == "https" and 443 or 80)
  port = port or def
  local host_header = (port == def) and host or (host .. ":" .. tostring(port))
  return scheme, host, port, path, host_header
end

-- Resolve a relative or absolute Location header against the current URL.
function Upstream.resolve_location(current, scheme, host, loc)
  if not loc or loc == "" then return nil end
  if loc:match("^https?://") then return loc end
  if loc:match("^//")        then return scheme .. ":" .. loc end
  if loc:match("^[^/]+:%d+/.") or loc:match("^[^/]+/") then
    return scheme .. "://" .. loc
  end
  if loc:sub(1, 1) == "/" then return scheme .. "://" .. host .. loc end
  local base = current:match("^(https?://[^?]+)")
  local dir  = base and base:match("(.*/)") or (scheme .. "://" .. host .. "/")
  return dir .. loc
end

-- ======================== Header Building (Detection Avoidance) ========================

-- Build upstream request headers that mimic the downstream client.
-- DETECTION AVOIDANCE: forwards all meaningful client headers so the
-- upstream provider sees traffic indistinguishable from a direct player.
--
-- client_headers: table of headers to forward (from ngx.req.get_headers()
--   or captured before timer). Pass nil to read from current request.
-- host_header: override Host (set per-hop during redirect following)
-- orig_url: used as Referer fallback
function Upstream.build_client_like_headers(host_header, orig_url, client_headers)
  local ch = client_headers or ngx.req.get_headers()
  local h = {
    ["Host"]                  = host_header,
    ["User-Agent"]            = ch["User-Agent"] or ch["user-agent"],
    ["Accept"]                = ch["Accept"] or ch["accept"],
    ["Accept-Language"]       = ch["Accept-Language"] or ch["accept-language"],
    ["Accept-Encoding"]       = ch["Accept-Encoding"] or ch["accept-encoding"],
    ["Referer"]               = ch["Referer"] or ch["referer"] or orig_url,
    ["Origin"]                = ch["Origin"] or ch["origin"],
    ["Cookie"]                = ch["Cookie"] or ch["cookie"],
    ["Authorization"]         = ch["Authorization"] or ch["authorization"],
    -- Emby/Jellyfin specific
    ["X-Playback-Session-Id"] = ch["X-Playback-Session-Id"] or ch["x-playback-session-id"],
    -- Conditional request headers (may be sent by seeking players)
    ["If-Range"]              = ch["If-Range"] or ch["if-range"],
    ["If-None-Match"]         = ch["If-None-Match"] or ch["if-none-match"],
    ["If-Modified-Since"]     = ch["If-Modified-Since"] or ch["if-modified-since"],
  }
  -- Strip nil/empty values
  local out = {}
  for k, v in pairs(h) do
    if v and v ~= "" then out[k] = v end
  end
  -- Ensure we always have a User-Agent (critical for detection avoidance)
  if not out["User-Agent"] then
    out["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
  end
  return out
end

-- ======================== Low-Level Connection ========================

local function connect_raw(host, port, scheme, ssl_verify, read_timeout_ms)
  local httpc = http.new()
  -- 5s connect/send. Read timeout: configurable for streaming reads.
  -- 0 = no timeout (legacy behavior).
  httpc:set_timeouts(5000, 5000, read_timeout_ms or 0)
  local ok, err = httpc:connect(host, port)
  if not ok then return nil, "connect_failed: " .. tostring(err) end
  if scheme == "https" then
    local ok2, err2 = httpc:ssl_handshake(nil, host, ssl_verify)
    if not ok2 then
      httpc:close()
      return nil, "ssl_failed: " .. tostring(err2)
    end
  end
  return httpc, nil
end

-- ======================== Centralized Request with Redirect Following ========================

-- Performs an HTTP request with automatic redirect following (up to max_redirects).
-- Returns: res, httpc, final_url  OR  nil, err_string, nil
--
-- The caller MUST close httpc when done (after consuming body).
-- This eliminates duplicated redirect loops across nocache/tee/head paths.
--
-- options.read_timeout_ms: per-request socket read timeout (ms). 0 = no timeout.
function Upstream.request(url, options)
  options = options or {}
  local method        = options.method or "GET"
  local max_redirects = options.max_redirects or 5
  local ssl_verify    = (options.ssl_verify ~= false)
  local read_timeout  = options.read_timeout_ms or 0

  local current_url = url
  local hops = 0
  local httpc, res, err

  while hops <= max_redirects do
    local scheme, host, port, path, host_header = Upstream.parse_url(current_url)
    if not scheme then return nil, "invalid_url_parse", nil end

    httpc, err = connect_raw(host, port, scheme, ssl_verify, read_timeout)
    if not httpc then return nil, err, nil end

    -- Prepare headers: clone to avoid mutation, set Host per-hop
    local headers = Utils.tclone(options.headers or {})
    headers["Host"] = host_header

    res, err = httpc:request({
      method  = method,
      path    = path,
      headers = headers,
    })

    if not res then
      httpc:close()
      return nil, "request_failed: " .. tostring(err), nil
    end

    -- Follow redirects (301, 302, 303, 307, 308)
    if res.status >= 300 and res.status < 400 and res.status ~= 304 then
      local loc = res.headers and (res.headers["Location"] or res.headers["location"])
      httpc:close()
      if not loc then return nil, "redirect_no_location", nil end
      local next_url = Upstream.resolve_location(current_url, scheme, host_header, loc)
      if not next_url then return nil, "redirect_resolve_failed", nil end
      current_url = next_url
      hops = hops + 1
    else
      -- Non-redirect response: return open connection for streaming
      return res, httpc, current_url
    end
  end

  return nil, "too_many_redirects", nil
end

-- ======================== Probe Origin (with persistent per-host method cache) ========================

-- Parse the metadata fields we care about from a probe response.
-- Returns a table with: size, content_type, etag, last_modified (any may be nil).
local function parse_probe_response(res)
  local meta = {}
  local hl = {}
  for k, v in pairs(res.headers or {}) do hl[string.lower(k)] = v end

  -- Size: prefer Content-Range total (from 206 to a Range request),
  -- fall back to Content-Length only when status is 200 (full body).
  -- For 206 responses without Content-Range, we don't extract size to
  -- avoid using the partial-byte length as the file size.
  if hl["content-range"] then
    local t = hl["content-range"]:match("/(%d+)$")
    if t then meta.size = tonumber(t) end
  end
  if not meta.size and res.status == 200 and hl["content-length"] then
    meta.size = tonumber(hl["content-length"])
  end

  meta.content_type  = hl["content-type"]
  meta.etag          = hl["etag"]
  meta.last_modified = hl["last-modified"]
  return meta
end

-- Probe origin for media metadata, using a persistent per-provider-host
-- method cache stored in `dict` (a lua_shared_dict, typically `probe_methods`).
--
-- Algorithm:
--   1. Look up cached method for the URL's host.
--   2. If cached as "GET_RANGE", skip HEAD; go straight to GET 0-0.
--   3. Otherwise (cached as "HEAD" or absent), try HEAD first.
--      On 2xx → return metadata. Cache "HEAD" if we hadn't seen this
--      provider before.
--   4. If HEAD failed (non-2xx, network error, redirect-chain failure),
--      try GET 0-0. On 2xx → return metadata. Update cache to "GET_RANGE"
--      (logs PROBE_METHOD_LEARNED if first time, PROBE_METHOD_CHANGED if
--      transitioning from HEAD→GET_RANGE).
--   5. If both methods fail end-to-end → log ERROR PROBE_FAILED and return
--      { ok = false }.
--
-- The cache uses exptime=0 (no expiry) so entries persist for container
-- lifetime. Container restart triggers re-discovery. Adaptive recovery is
-- automatic: a previously-cached HEAD that stops working will silently
-- transition to GET_RANGE on the next probe.
--
-- dict: optional lua_shared_dict for caching. If nil, no caching is done
--   (HEAD-then-GET fallback still works, just without persistence).
function Upstream.probe_origin(url, hdrs, ssl_verify, dict)
  local scheme, host, port = Upstream.parse_url(url)
  if not scheme then
    ngx.log(ngx.ERR, "[streamcache] PROBE_FAILED: invalid_url ", tostring(url))
    return { ok = false, reason = "invalid_url" }
  end

  local probe_key = "pm:" .. host .. ":" .. tostring(port)
  local cached = nil
  if dict then cached = dict:get(probe_key) end

  -- Helper: issue a probe with a given method/range and return:
  --   true, meta_table  on 2xx success
  --   false, nil        on non-2xx, network error, or invalid response
  -- Always closes httpc before returning (we don't consume the body).
  local function attempt_probe(method, range_value)
    local h = {}
    if hdrs then for k, v in pairs(hdrs) do h[k] = v end end
    if range_value then h["Range"] = range_value end

    local res, httpc, _ = Upstream.request(url, {
      method     = method,
      headers    = h,
      ssl_verify = ssl_verify,
      -- Probes are short; the default 0 (no read timeout) suffices.
      -- Connect/send timeouts in connect_raw still apply (5s each).
    })

    if not res then return false, nil end

    -- Probes don't consume the response body; close the connection now.
    pcall(function() if httpc then httpc:close() end end)

    if res.status >= 200 and res.status < 300 then
      return true, parse_probe_response(res)
    end
    return false, nil
  end

  -- Step 1+3: try HEAD unless we already know provider rejects it.
  if cached ~= "GET_RANGE" then
    local ok, meta = attempt_probe("HEAD", nil)
    if ok then
      if dict and cached == nil then
        dict:set(probe_key, "HEAD", 0)  -- exptime=0: persists for container lifetime
        ngx.log(ngx.INFO, "[streamcache] PROBE_METHOD_LEARNED: ",
                host, ":", port, " = HEAD")
      end
      meta.ok = true
      return meta
    end
    -- HEAD failed end-to-end; fall through to GET 0-0
  end

  -- Step 4: try GET 0-0 (works for providers that block HEAD).
  local ok, meta = attempt_probe("GET", "bytes=0-0")
  if ok then
    if dict and cached ~= "GET_RANGE" then
      dict:set(probe_key, "GET_RANGE", 0)
      if cached == nil then
        ngx.log(ngx.INFO, "[streamcache] PROBE_METHOD_LEARNED: ",
                host, ":", port, " = GET_RANGE")
      else
        ngx.log(ngx.INFO, "[streamcache] PROBE_METHOD_CHANGED: ",
                host, ":", port, " HEAD -> GET_RANGE")
      end
    end
    meta.ok = true
    return meta
  end

  -- Step 5: both methods failed.
  ngx.log(ngx.ERR, "[streamcache] PROBE_FAILED: ", host, ":", port,
          " all methods exhausted")
  return { ok = false, reason = "probe_failed" }
end

-- Ensure the probe method for this URL's host is discovered. Used at MISS
-- time to populate the cache before the first stale check ever fires.
-- No-op if already cached. Discards the metadata (we just want the side
-- effect of the dict being populated).
function Upstream.ensure_method_known(url, hdrs, ssl_verify, dict)
  if not dict then return end
  local scheme, host, port = Upstream.parse_url(url)
  if not scheme then return end
  local probe_key = "pm:" .. host .. ":" .. tostring(port)
  if dict:get(probe_key) ~= nil then return end
  -- Run discovery (probe_origin populates dict as a side effect)
  Upstream.probe_origin(url, hdrs, ssl_verify, dict)
end

-- ======================== Header Probing (legacy shim) ========================

-- Probe expected total size using HEAD or GET 0-0 with adaptive caching.
-- Returns: total_size (number or nil), accept_ranges (boolean)
-- Now backed by Upstream.probe_origin so all probe paths share the
-- per-host method cache.
function Upstream.probe_total_with_head(url, hdrs, ssl_verify, dict)
  local meta = Upstream.probe_origin(url, hdrs, ssl_verify, dict)
  if not meta.ok then return nil, false end
  -- We got a 2xx response; any modern origin that supports our probe
  -- method also supports byte-range requests, so accept_ranges=true.
  return meta.size, true
end

return Upstream
