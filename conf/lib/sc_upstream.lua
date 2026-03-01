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

local function connect_raw(host, port, scheme, ssl_verify)
  local httpc = http.new()
  httpc:set_timeouts(5000, 5000, 0) -- 5s connect/send, infinite read (streaming)
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
function Upstream.request(url, options)
  options = options or {}
  local method        = options.method or "GET"
  local max_redirects = options.max_redirects or 5
  local ssl_verify    = (options.ssl_verify ~= false)

  local current_url = url
  local hops = 0
  local httpc, res, err

  while hops <= max_redirects do
    local scheme, host, port, path, host_header = Upstream.parse_url(current_url)
    if not scheme then return nil, "invalid_url_parse", nil end

    httpc, err = connect_raw(host, port, scheme, ssl_verify)
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

-- ======================== Header Probing ========================

-- Fetch origin response headers via HEAD (falls back to GET 0-0 if HEAD
-- returns 405). Used by followers to get Content-Type, ETag, etc.
-- Returns a table of lowercased header keys → values.
function Upstream.fetch_origin_headers(url, hdrs, ssl_verify)
  local scheme, host, port, path, host_header = Upstream.parse_url(url)
  if not scheme then return {} end

  local httpc = http.new()
  httpc:set_timeouts(3000, 3000, 3000)
  local ok = httpc:connect(host, port)
  if not ok then return {} end
  if scheme == "https" then
    local ok2 = httpc:ssl_handshake(nil, host, ssl_verify ~= false)
    if not ok2 then httpc:close(); return {} end
  end

  local headers = Utils.tclone(hdrs or {})
  headers["Host"] = host_header
  if not headers["User-Agent"] then
    headers["User-Agent"] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
  end

  local res = select(1, httpc:request{ method = "HEAD", path = path, headers = headers })
  if (not res) or res.status == 405 then
    headers["Range"] = headers["Range"] or "bytes=0-0"
    res = select(1, httpc:request{ method = "GET", path = path, headers = headers })
  end
  if not res then httpc:close(); return {} end

  local out = {}
  for k, v in pairs(res.headers or {}) do out[string.lower(k)] = v end
  httpc:close()
  return out
end

-- Probe expected total size using HEAD (preferred) or 0-0 GET fallback.
-- Returns: total_size (number or nil), accept_ranges (boolean)
function Upstream.probe_total_with_head(url, hdrs, ssl_verify)
  local oh = Upstream.fetch_origin_headers(url, hdrs, ssl_verify)
  local total, accept_ranges = nil, false
  if oh["content-range"] then
    local t0 = oh["content-range"]:match("/(%d+)$")
    if t0 then total = tonumber(t0) end
  elseif oh["content-length"] then
    total = tonumber(oh["content-length"])
  end
  if oh["accept-ranges"] == "bytes" then accept_ranges = true end
  return total, accept_ranges
end

return Upstream
