-- streamcache.lua
-- Single-pass tee + redirect-follow; never leak Location/30x to clients.

local ngx  = ngx
local os   = os
local http = require "resty.http"

-- ===================== Config (via env) =====================
local VERBOSE               = (os.getenv("LOG_VERBOSE") == "1")
local RANGE_TEE_THRESHOLD   = tonumber(os.getenv("RANGE_TEE_THRESHOLD") or "") or (5 * 1024 * 1024) -- 5 MiB
local PROGRESS_FLUSH_BYTES  = tonumber(os.getenv("PROGRESS_FLUSH_BYTES") or "") or 262144                -- 256 KiB
local FOLLOWER_WAIT_MAX     = tonumber(os.getenv("FOLLOWER_WAIT_MAX") or "") or 600                      -- seconds
local FOLLOWER_POLL_MS      = tonumber(os.getenv("FOLLOWER_POLL_MS") or "") or 50                        -- ms
local SSL_VERIFY            = (os.getenv("DISABLE_SSL_VERIFY") ~= "1")

-- ===================== Helpers =====================
local function b2human(n)
  if not n or n < 0 then return "0 B" end
  local u = {"B","KB","MB","GB","TB","PB"}; local i=1
  while n>=1024 and i<#u do n = n/1024; i = i+1 end
  return string.format("%.2f %s", n, u[i])
end

local function write_meta(key, size, last)
  local d = "/var/cache/streamcache/meta"
  os.execute("mkdir -p " .. d)
  local f = io.open(d .. "/" .. key .. ".meta", "wb")
  if not f then return end
  f:write("size=" .. tostring(size or 0) .. "\n")
  f:write("last_access=" .. tostring(last or ngx.now()) .. "\n")
  f:close()
end
local function b64url_decode(s)
  s = s:gsub("-", "+"):gsub("_", "/")
  local pad = #s % 4
  if pad > 0 then s = s .. string.rep("=", 4 - pad) end
  return ngx.decode_base64(s)
end

local function file_exists(p) local f=io.open(p,"rb"); if f then f:close(); return true end return false end
local function file_size(p)  local f=io.open(p,"rb"); if not f then return 0 end local sz=f:seek("end"); f:close(); return sz or 0 end
local function key_from_url(u) return ngx.md5(u) end

-- URL parsing
local function split_hostport(authority, scheme)
  if not authority or authority == "" then return nil, nil end
  local h,p = authority:match("^%[(.-)%]:(%d+)$"); if h then return h, tonumber(p) end
  h = authority:match("^%[(.-)%]$"); if h then return h, (scheme=="https" and 443 or 80) end
  h,p = authority:match("^([^:]+):(%d+)$"); if h then return h, tonumber(p) end
  return authority, (scheme=="https" and 443 or 80)
end
local function parse_url(u)
  if not u then return nil end
  local scheme, rest = u:match("^(https?)://(.+)$"); if not scheme then return nil end
  local authority, path = rest:match("^([^/]+)(/.*)$"); authority=authority or rest; path=path or "/"
  local host, port = split_hostport(authority, scheme); if not host then return nil end
  local def = (scheme=="https" and 443 or 80); port = port or def
  local host_header = (port==def) and host or (host .. ":" .. tostring(port))
  return scheme, host, port, path, host_header
end

local function resolve_location(current, scheme, host, loc)
  if not loc or loc == "" then return nil end
  if loc:match("^https?://") then return loc end
  if loc:match("^//") then return scheme .. ":" .. loc end
  if loc:match("^[^/]+:%d+/.") or loc:match("^[^/]+/") then return scheme .. "://" .. loc end
  if loc:sub(1,1) == "/" then return scheme .. "://" .. host .. loc end
  local base = current:match("^(https?://[^?]+)")
  local dir = base and base:match("(.*/)") or (scheme .. "://" .. host .. "/")
  return dir .. loc
end

local function tclone(t) local o={}; if not t then return o end for k,v in pairs(t) do o[k]=v end return o end

local function build_client_like_headers(host_header, orig_url)
  local ch = ngx.req.get_headers()
  local h = {
    ["Host"]            = host_header,
    ["User-Agent"]      = ch["User-Agent"],
    ["Accept"]          = ch["Accept"],
    ["Accept-Language"] = ch["Accept-Language"],
    ["Accept-Encoding"] = ch["Accept-Encoding"],
    ["Referer"]         = ch["Referer"] or orig_url,
    ["Origin"]          = ch["Origin"],
    ["Cookie"]          = ch["Cookie"],
    ["Authorization"]   = ch["Authorization"],
  }
  for k,v in pairs(tclone(h)) do if v == nil or v == "" then h[k]=nil end end
  return h
end

-- Fetch a few headers (for follower friendliness)
local function fetch_origin_headers(final_url, hdrs)
  local scheme, host, port, path, host_header = parse_url(final_url)
  if not scheme then return {} end
  local httpc = http.new()
  httpc:set_timeouts(3000, 3000, 3000)
  local ok = httpc:connect(host, port); if not ok then return {} end
  if scheme == "https" then
    local ok2 = httpc:ssl_handshake(nil, host, SSL_VERIFY)
    if not ok2 then httpc:close(); return {} end
  end
  local headers = tclone(hdrs or {}); headers["Host"] = host_header
  if not headers["User-Agent"] then headers["User-Agent"] = "EmbyStreamCache/1.0" end
  local res = select(1, httpc:request{ method="HEAD", path=path, headers=headers })
  if (not res) or res.status == 405 then
    headers["Range"] = headers["Range"] or "bytes=0-0"
    res = select(1, httpc:request{ method="GET", path=path, headers=headers })
  end
  if not res then httpc:close(); return {} end
  local out = {}; for k,v in pairs(res.headers or {}) do out[string.lower(k)] = v end
  httpc:close(); return out
end

-- ===================== Shared dicts =====================
local inflight  = ngx.shared.inflight
local access    = ngx.shared.access
local resolved  = ngx.shared.resolved   -- kept for future hints
local progress  = ngx.shared.progress
local totals    = ngx.shared.totals

-- ===================== No-cache streamer (far-seek passthrough & HEAD) =====================
local function stream_no_cache(url, normalize_200_when_no_client_range)
  local scheme, host, port, path, host_header = parse_url(url)
  if not scheme then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local httpc = http.new()
  httpc:set_timeouts(5000, 5000, 0)

  local function connect_to(h, p, sch)
    local ok, err = httpc:connect(h, p)
    if not ok then ngx.log(ngx.WARN, "[streamcache] nocache connect failed: ", tostring(err)); return nil end
    if sch == "https" then
      local ok2, err2 = httpc:ssl_handshake(nil, h, SSL_VERIFY)
      if not ok2 then ngx.log(ngx.WARN, "[streamcache] nocache TLS failed: ", tostring(err2)); return nil end
    end
    return true
  end
  if not connect_to(host, port, scheme) then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local ch = ngx.req.get_headers()
  local headers = build_client_like_headers(host_header, url)
  headers["Range"] = ch["Range"] -- passthrough range as-is

  local hops, res, rerr = 0, nil, nil
  while true do
    res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    if not res then ngx.log(ngx.WARN, "[streamcache] nocache request failed: ", tostring(rerr)); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
    if (res.status == 301 or res.status == 302 or res.status == 303 or res.status == 307 or res.status == 308) then
      if hops >= 5 then ngx.log(ngx.WARN, "[streamcache] nocache too many redirects"); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      local loc = res.headers and (res.headers["Location"] or res.headers["location"])
      httpc:close()
      local next_url = resolve_location(url, scheme, host_header, loc)
      if not next_url then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      url = next_url
      scheme, host, port, path, host_header = parse_url(url)
      if not scheme then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not connect_to(host, port, scheme) then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      headers["Host"] = host_header
      hops = hops + 1
    else
      break
    end
  end

  local client_sent_range = (ch["Range"] ~= nil)
  local hl = {}; for k,v in pairs(res.headers or {}) do hl[string.lower(k)] = v end
  local total_size = nil
  if hl["content-range"] then local s0,e0,t0 = hl["content-range"]:match("^bytes%s+(%d+)%-(%d+)%/(%d+)$"); if t0 then total_size = tonumber(t0) end end
  if not total_size and hl["content-length"] and res.status == 200 then total_size = tonumber(hl["content-length"]) end

  local hcopy = {}
  for k,v in pairs(res.headers or {}) do
    local kl = string.lower(k)
    if kl ~= "transfer-encoding" and kl ~= "connection" and kl ~= "keep-alive"
       and kl ~= "proxy-authenticate" and kl ~= "proxy-authorization"
       and kl ~= "te" and kl ~= "trailer" and kl ~= "upgrade"
       and kl ~= "location" then  -- never expose redirects
      hcopy[k] = v
    end
  end
  hcopy["X-Cache"] = "MISS"
  hcopy["Accept-Ranges"] = "bytes"

  if normalize_200_when_no_client_range and (not client_sent_range) and res.status == 206 then
    hcopy["Content-Range"] = nil
    if total_size and total_size > 0 then hcopy["Content-Length"] = tostring(total_size) else hcopy["Content-Length"] = nil end
    ngx.status = ngx.HTTP_OK
  else
    ngx.status = res.status
  end
  for k,v in pairs(hcopy) do ngx.header[k]=v end
  ngx.send_headers()

  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] nocache read error: ", tostring(cerr)); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      local okp = pcall(ngx.print, chunk)
      if okp then ngx.flush(true) end
    end
  elseif res.body then
    local okp = pcall(ngx.print, res.body); if okp then ngx.flush(true) end
  end

  httpc:close()
  return
end

-- HEAD helper: follow redirects; never expose Location; no body
local function head_no_cache(url)
  local scheme, host, port, path, host_header = parse_url(url)
  if not scheme then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
  local httpc = http.new()
  httpc:set_timeouts(3000, 3000, 3000)

  local function connect_to(h, p, sch)
    local ok, err = httpc:connect(h, p)
    if not ok then ngx.log(ngx.WARN,"[streamcache] head connect failed: ",tostring(err)); return nil end
    if sch == "https" then
      local ok2, err2 = httpc:ssl_handshake(nil, h, SSL_VERIFY)
      if not ok2 then ngx.log(ngx.WARN,"[streamcache] head TLS failed: ",tostring(err2)); return nil end
    end
    return true
  end
  if not connect_to(host, port, scheme) then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local headers = build_client_like_headers(host_header, url)
  local hops, res, rerr = 0, nil, nil
  while true do
    res, rerr = httpc:request{ method = "HEAD", path = path, headers = headers }
    if (not res) or res.status == 405 then
      headers["Range"] = headers["Range"] or "bytes=0-0"
      res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    end
    if not res then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
    if (res.status == 301 or res.status == 302 or res.status == 303 or res.status == 307 or res.status == 308) then
      if hops >= 5 then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      local loc = res.headers and (res.headers["Location"] or res.headers["location"])
      httpc:close()
      local next_url = resolve_location(url, scheme, host_header, loc)
      if not next_url then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      url = next_url
      scheme, host, port, path, host_header = parse_url(url)
      if not scheme then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not connect_to(host, port, scheme) then httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      headers["Host"] = host_header
      hops = hops + 1
    else
      break
    end
  end

  local hcopy = {}
  for k,v in pairs(res.headers or {}) do
    local kl = string.lower(k)
    if kl ~= "transfer-encoding" and kl ~= "connection" and kl ~= "keep-alive"
       and kl ~= "proxy-authenticate" and kl ~= "proxy-authorization"
       and kl ~= "te" and kl ~= "trailer" and kl ~= "upgrade"
       and kl ~= "location" then
      hcopy[k]=v
    end
  end
  hcopy["Accept-Ranges"] = "bytes"
  ngx.status = ngx.HTTP_OK
  for k,v in pairs(hcopy) do ngx.header[k]=v end
  ngx.send_headers()
  httpc:close()
  return
end

-- ===================== Tee: upstream -> client + file =====================
local function tee_stream(final_url, dest_path, key, use_range_0, orig_url_for_ref)
  final_url = (final_url or ""):gsub("^%s+", ""):gsub("[%s\r\n]+$", "")
  local scheme, host, port, path, host_header = parse_url(final_url)
  if not scheme then ngx.log(ngx.WARN,"[streamcache] tee parse failed url=",tostring(final_url)); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local httpc = http.new()
  httpc:set_timeouts(5000, 5000, 0)

  local function connect_to(h, p, sch)
    local ok, err = httpc:connect(h, p)
    if not ok then ngx.log(ngx.WARN,"[streamcache] tee connect failed: ",tostring(err)); return nil end
    if sch == "https" then
      local ok2, err2 = httpc:ssl_handshake(nil, h, SSL_VERIFY)
      if not ok2 then ngx.log(ngx.WARN,"[streamcache] tee TLS failed: ",tostring(err2)); return nil end
    end
    return true
  end
  if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local ch = ngx.req.get_headers()
  local headers = build_client_like_headers(host_header, orig_url_for_ref)
  if ch["Range"] and ch["Range"] ~= "" then headers["Range"] = ch["Range"]
  elseif use_range_0 then headers["Range"] = "bytes=0-"
  else headers["Range"] = nil end

  local hops, res, rerr = 0, nil, nil
  while true do
    res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    if not res then ngx.log(ngx.WARN,"[streamcache] tee request failed: ",tostring(rerr)); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
    if (res.status == 301 or res.status == 302 or res.status == 303 or res.status == 307 or res.status == 308) then
      if hops >= 5 then ngx.log(ngx.WARN,"[streamcache] tee too many redirects"); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      local loc = res.headers and (res.headers["Location"] or res.headers["location"])
      httpc:close()
      local next_url = resolve_location(final_url, scheme, host_header, loc)
      if not next_url then inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      final_url = next_url
      scheme, host, port, path, host_header = parse_url(final_url)
      if not scheme then inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      headers["Host"] = host_header
      hops = hops + 1
    else
      break
    end
  end

  -- Range-hostile fallback: single retry without Range
  if not (res.status == 200 or res.status == 206) then
    if headers["Range"] and (res.status == 416 or res.status == 400 or res.status == 403) then
      headers["Range"] = nil
      res:read_body()
      httpc:close()
      if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    end
    if not res or not (res.status == 200 or res.status == 206) then
      ngx.log(ngx.WARN,"[streamcache] tee status not OK: ", res and res.status or tostring(rerr))
      httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY)
    end
  end

  -- Parse totals
  local hl = {}; for k,v in pairs(res.headers or {}) do hl[string.lower(k)] = v end
  local total_size = nil
  if hl["content-range"] then local s0,e0,t0 = hl["content-range"]:match("^bytes%s+(%d+)%-(%d+)%/(%d+)$"); if t0 then total_size = tonumber(t0) end end
  if not total_size and hl["content-length"] and res.status == 200 then total_size = tonumber(hl["content-length"]) end
  if total_size and total_size > 0 then totals:set(key, total_size, 900) end

  -- Prepare response headers to client; normalize 206->200 if client didn't ask Range
  local client_sent_range = (ch["Range"] ~= nil)
  local hcopy = {}
  for k,v in pairs(res.headers or {}) do
    local kl = string.lower(k)
    if kl ~= "transfer-encoding" and kl ~= "connection" and kl ~= "keep-alive"
       and kl ~= "proxy-authenticate" and kl ~= "proxy-authorization"
       and kl ~= "te" and kl ~= "trailer" and kl ~= "upgrade"
       and kl ~= "location" then
      hcopy[k] = v
    end
  end
  hcopy["Accept-Ranges"] = "bytes"

  if (not client_sent_range) and res.status == 206 then
    hcopy["Content-Range"] = nil
    if total_size and total_size > 0 then hcopy["Content-Length"] = tostring(total_size) else hcopy["Content-Length"] = nil end
    ngx.status = ngx.HTTP_OK
  else
    ngx.status = res.status
  end
  for k,v in pairs(hcopy) do ngx.header[k] = v end
  ngx.send_headers()

  -- Open temp file & tee (with no-cache fallback)
  os.execute("mkdir -p /var/cache/streamcache/tmp")
  local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"
  local f = io.open(tmp, "wb")
  if not f then
    ngx.log(ngx.ERR,"[streamcache] tee cannot open temp: ", tmp, " ; falling back to streaming without cache")
    if res.body_reader then
      while true do
        local chunk, cerr = res.body_reader(32768)
        if cerr then ngx.log(ngx.WARN,"[streamcache] read error (nocache fallback): ", tostring(cerr)); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
        if not chunk then break end
        local okp = pcall(ngx.print, chunk); if okp then ngx.flush(true) end
      end
    elseif res.body then
      local okp = pcall(ngx.print, res.body); if okp then ngx.flush(true) end
    end
    httpc:close(); inflight:delete(key); return
  end

  local total = 0
  local last_report = 0
  local client_alive = true

  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] tee read error: ",tostring(cerr)); f:close(); os.remove(tmp); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      total = total + #chunk
      f:write(chunk)
      if (total - last_report) >= PROGRESS_FLUSH_BYTES then
        f:flush(); progress:set(key, total, 900); last_report = total
      end
      if client_alive then local okp = pcall(ngx.print, chunk); if not okp then client_alive=false else ngx.flush(true) end end
    end
  elseif res.body then
    total = #res.body
    f:write(res.body); f:flush(); progress:set(key, total, 900)
    if client_alive then pcall(ngx.print, res.body); ngx.flush(true) end
  end

  f:flush(); progress:set(key, total, 900); f:close(); httpc:close()

  if total > 0 then
    os.rename(tmp, dest_path)
    local ts = ngx.now(); write_meta(key, total, ts); access:set(key, ts)
    if VERBOSE then ngx.log(ngx.NOTICE,"[streamcache] tee complete key=", key, " size=", b2human(total)) end
  else
    os.remove(tmp); ngx.log(ngx.WARN,"[streamcache] tee produced 0 bytes; not committing key=", key)
  end

  inflight:delete(key)
  return
end

-- Tee with near-start window: fetch 0-; cache full; serve client slice only
local function tee_stream_range_window(final_url, dest_path, key, client_start, client_end, orig_url_for_ref)
  final_url = (final_url or ""):gsub("^%s+", ""):gsub("[%s\r\n]+$", "")
  local scheme, host, port, path, host_header = parse_url(final_url)
  if not scheme then ngx.log(ngx.WARN,"[streamcache] tee-window parse failed url=",tostring(final_url)); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local httpc = http.new()
  httpc:set_timeouts(5000, 5000, 0)

  local function connect_to(h, p, sch)
    local ok, err = httpc:connect(h, p)
    if not ok then ngx.log(ngx.WARN,"[streamcache] tee-window connect failed: ",tostring(err)); return nil end
    if sch == "https" then
      local ok2, err2 = httpc:ssl_handshake(nil, h, SSL_VERIFY)
      if not ok2 then ngx.log(ngx.WARN,"[streamcache] tee-window TLS failed: ",tostring(err2)); return nil end
    end
    return true
  end
  if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  local ch = ngx.req.get_headers()
  local headers = build_client_like_headers(host_header, orig_url_for_ref)
  headers["Range"] = ch["Range"] or "bytes=0-"

  local hops, res, rerr = 0, nil, nil
  while true do
    res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    if not res then ngx.log(ngx.WARN,"[streamcache] tee-window request failed: ",tostring(rerr)); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
    if (res.status == 301 or res.status == 302 or res.status == 303 or res.status == 307 or res.status == 308) then
      if hops >= 5 then ngx.log(ngx.WARN,"[streamcache] tee-window too many redirects"); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      local loc = res.headers and (res.headers["Location"] or res.headers["location"])
      httpc:close()
      local next_url = resolve_location(final_url, scheme, host_header, loc)
      if not next_url then inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      final_url = next_url
      scheme, host, port, path, host_header = parse_url(final_url)
      if not scheme then inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      headers["Host"] = host_header
      hops = hops + 1
    else
      break
    end
  end

  -- Range-hostile fallback
  if not (res.status == 200 or res.status == 206) then
    if headers["Range"] and (res.status == 416 or res.status == 400 or res.status == 403) then
      headers["Range"] = nil
      res:read_body()
      httpc:close()
      if not connect_to(host, port, scheme) then httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      res, rerr = httpc:request{ method = "GET", path = path, headers = headers }
    end
    if not res or not (res.status == 200 or res.status == 206) then
      ngx.log(ngx.WARN,"[streamcache] tee-window status not OK: ", res and res.status or tostring(rerr))
      httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY)
    end
  end

  -- Determine total size
  local headers_l = {}; for k,v in pairs(res.headers or {}) do headers_l[string.lower(k)] = v end
  local total_size = nil
  if headers_l["content-range"] then
    local s0,e0,t0 = headers_l["content-range"]:match("^bytes%s+(%d+)%-(%d+)%/(%d+)$")
    if s0 and tonumber(s0) == 0 and e0 and t0 then total_size = tonumber(t0) end
  end
  if not total_size and headers_l["content-length"] and res.status == 200 then
    total_size = tonumber(headers_l["content-length"])
  end
  if total_size and total_size > 0 then totals:set(key, total_size, 900) end

  if not total_size or total_size <= 0 then
    httpc:close(); inflight:delete(key)
    return stream_no_cache(final_url, false)
  end

  -- Compute outbound client range to serve
  local send_start = math.max(0, tonumber(client_start) or 0)
  local send_end   = tonumber(client_end)
  if not send_end or send_end > (total_size - 1) then send_end = total_size - 1 end
  if send_start > send_end then
    httpc:close(); inflight:delete(key)
    ngx.header["Content-Range"] = string.format("bytes */%d", total_size)
    return ngx.exit(ngx.HTTP_REQUESTED_RANGE_NOT_SATISFIABLE)
  end
  local send_len = (send_end - send_start + 1)

  -- Prepare 206 headers to client
  local hcopy = {}
  for k,v in pairs(res.headers or {}) do
    local kl = string.lower(k)
    if kl == "content-type" or kl == "etag" or kl == "last-modified" or kl == "content-disposition" then
      hcopy[k] = v
    end
  end
  hcopy["Accept-Ranges"] = "bytes"
  hcopy["Content-Range"] = string.format("bytes %d-%d/%d", send_start, send_end, total_size)
  hcopy["Content-Length"] = tostring(send_len)
  ngx.status = ngx.HTTP_PARTIAL_CONTENT
  for k,v in pairs(hcopy) do ngx.header[k] = v end
  ngx.send_headers()

  -- Open temp & stream windowed slice while caching full body (with no-cache fallback)
  os.execute("mkdir -p /var/cache/streamcache/tmp")
  local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"
  local f = io.open(tmp, "wb")
  if not f then
    ngx.log(ngx.ERR,"[streamcache] tee-window cannot open temp: ", tmp, " ; falling back to streaming without cache")
    local pos = 0
    local function maybe_send_slice(chunk, chunk_start)
      local chunk_len = #chunk
      local chunk_end = chunk_start + chunk_len - 1
      if chunk_end < send_start or chunk_start > send_end then return 0 end
      local from = math.max(send_start - chunk_start, 0) + 1
      local to   = math.min(send_end - chunk_start + 1, chunk_len)
      local slice = chunk:sub(from, to)
      if #slice > 0 then local okp = pcall(ngx.print, slice); if okp then ngx.flush(true) end end
      return #slice
    end
    if res.body_reader then
      while true do
        local chunk, cerr = res.body_reader(32768)
        if cerr then ngx.log(ngx.WARN,"[streamcache] read error (window nocache fallback): ", tostring(cerr)); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
        if not chunk then break end
        maybe_send_slice(chunk, pos)
        pos  = pos + #chunk
      end
    elseif res.body then
      maybe_send_slice(res.body, pos)
    end
    httpc:close(); inflight:delete(key); return
  end
  local total_written, last_report, sent, pos = 0, 0, 0, 0
  local function maybe_send_slice(chunk, chunk_start)
    local chunk_len = #chunk
    local chunk_end = chunk_start + chunk_len - 1
    if chunk_end < send_start or chunk_start > send_end then return 0 end
    local from = math.max(send_start - chunk_start, 0) + 1
    local to   = math.min(send_end - chunk_start + 1, chunk_len)
    local slice = chunk:sub(from, to)
    if #slice > 0 then local okp = pcall(ngx.print, slice); if okp then ngx.flush(true) end end
    return #slice
  end
  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] tee-window read error: ",tostring(cerr)); f:close(); os.remove(tmp); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      total_written = total_written + #chunk
      f:write(chunk)
      if (total_written - last_report) >= PROGRESS_FLUSH_BYTES then
        f:flush(); progress:set(key, total_written, 900); last_report = total_written
      end
      sent = sent + maybe_send_slice(chunk, pos)
      pos  = pos + #chunk
    end
  elseif res.body then
    local chunk = res.body
    total_written = #chunk
    f:write(chunk); f:flush(); progress:set(key, total_written, 900)
    sent = sent + maybe_send_slice(chunk, pos)
    pos  = pos + #chunk
  end

  f:flush(); progress:set(key, total_written, 900); f:close(); httpc:close()

  if total_written > 0 then
    os.rename(tmp, dest_path)
    local ts = ngx.now(); write_meta(key, total_written, ts); access:set(key, ts)
    if VERBOSE then ngx.log(ngx.NOTICE,"[streamcache] tee-window complete key=", key, " size=", b2human(total_written), " served=", tostring(sent), "B range=", tostring(send_start), "-", tostring(send_end)) end
  else
    os.remove(tmp); ngx.log(ngx.WARN,"[streamcache] tee-window produced 0 bytes; not committing key=", key)
  end

  inflight:delete(key)
  return
end

-- ===================== Follower (serve from .part) =====================
local function serve_from_part(final_url, key, req_start, req_end)
  local files_dir = "/var/cache/streamcache/files"
  local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"

  if file_exists(files_dir .. "/" .. key) then
    ngx.header["X-Cache"] = "HIT"
    return ngx.exec("/cache/" .. key)
  end
  local f = io.open(tmp, "rb")
  if not f then return nil, "no_part" end

  local total_size = totals:get(key)
  local waited = 0
  while not total_size and waited < 2000 do ngx.sleep(0.05); waited = waited + 50; total_size = totals:get(key) end
  if not total_size then f:close(); return nil, "no_total" end
  total_size = tonumber(total_size)

  local start = tonumber(req_start) or 0
  local stop  = (req_end and req_end ~= "") and tonumber(req_end) or (total_size - 1)
  if stop > (total_size - 1) then stop = total_size - 1 end
  if start > stop or start > total_size - 1 then
    f:close(); ngx.header["Content-Range"] = string.format("bytes */%d", total_size)
    return ngx.exit(ngx.HTTP_REQUESTED_RANGE_NOT_SATISFIABLE)
  end

  local oh = fetch_origin_headers(final_url, build_client_like_headers(nil, final_url))
  ngx.status = ngx.HTTP_PARTIAL_CONTENT
  ngx.header["X-Cache"] = "FOLLOW"
  ngx.header["Accept-Ranges"] = "bytes"
  ngx.header["Content-Range"] = string.format("bytes %d-%d/%d", start, stop, total_size)
  ngx.header["Content-Length"] = tostring(stop - start + 1)
  if oh["content-type"]        then ngx.header["Content-Type"]        = oh["content-type"] end
  if oh["etag"]                then ngx.header["ETag"]                = oh["etag"] end
  if oh["last-modified"]       then ngx.header["Last-Modified"]       = oh["last-modified"] end
  if oh["content-disposition"] then ngx.header["Content-Disposition"] = oh["content-disposition"] end
  ngx.send_headers()

  local deadline = ngx.now() + FOLLOWER_WAIT_MAX
  local sent, needed, current_offset = 0, (stop - start + 1), start
  f:seek("set", start)

  local function available_bytes()
    local p = progress:get(key) or 0
    local avail = p - current_offset
    if avail < 0 then avail = 0 end
    local max_need = (stop + 1) - current_offset
    if avail > max_need then avail = max_need end
    return avail
  end
  while sent < needed do
    local avail = available_bytes()
    if avail > 0 then
      local chunk_sz = math.min(avail, 8192)
      local chunk = f:read(chunk_sz)
      if not chunk or #chunk == 0 then
        ngx.sleep(FOLLOWER_POLL_MS / 1000)
      else
        local okp = pcall(ngx.print, chunk); if not okp then f:close(); return ngx.exit(ngx.HTTP_OK) end
        ngx.flush(true)
        sent = sent + #chunk
        current_offset = current_offset + #chunk
      end
    else
      if ngx.now() > deadline then
        f:close(); ngx.log(ngx.WARN,"[streamcache] follower timeout key=", key, " offset=", tostring(current_offset))
        return ngx.exit(ngx.HTTP_GATEWAY_TIMEOUT)
      end
      ngx.sleep(FOLLOWER_POLL_MS / 1000)
    end
  end
  f:close()
  return
end

-- ===================== Entry (HEAD-safe + policy) =====================

-- Decode Base64URL
local b64 = ngx.var.b64
local orig = b64 and b64url_decode(b64) or nil
if not orig or not orig:match("^https?://") then ngx.log(ngx.WARN,"[streamcache] bad request (invalid or non-http URL)"); return ngx.exit(ngx.HTTP_BAD_REQUEST) end
orig = orig:gsub("^%s+",""):gsub("[%s\r\n]+$","")

-- Optional SSRF allowlist
local allowed = os.getenv("ALLOWED_HOSTS")
if allowed and allowed ~= "" then
  local host = orig:match("^https?://([^/]+)")
  local ok=false; for h in allowed:gmatch("[^,%s]+") do if host==h then ok=true; break end end
  if not ok then ngx.status=ngx.HTTP_FORBIDDEN; ngx.say("Host not allowed"); return end
end
local method = ngx.req.get_method()
if method ~= "GET" then
  -- HEAD/OPTIONS/etc: no tee, no inflight; never expose 30x
  return head_no_cache(orig)
end

local files_dir = "/var/cache/streamcache/files"
os.execute("mkdir -p " .. files_dir .. " /var/cache/streamcache/tmp /var/cache/streamcache/meta")
local key = key_from_url(orig)
local final_path = files_dir .. "/" .. key

local function update_last_access()
  local ts = ngx.now(); access:set(key, ts); write_meta(key, file_size(final_path), ts)
end

-- HIT (with zero-byte guard)
if file_exists(final_path) then
  local sz = file_size(final_path)
  if sz <= 0 then
    os.remove(final_path); os.remove("/var/cache/streamcache/meta/"..key..".meta"); inflight:delete(key)
  else
    ngx.header["X-Cache"] = "HIT"
    if VERBOSE then ngx.log(ngx.INFO,"[streamcache] HIT key=", key, " size=", b2human(sz)) end
    update_last_access()
    return ngx.exec("/cache/" .. key)
  end
end
-- Decide path based on client Range
local req_range = ngx.req.get_headers()["Range"]
local use_range_0 = false
local near_start, near_end = nil, nil

if not req_range or req_range == "" then
  -- No Range from client: behave like a player upstream (Range:0-), normalize to 200 for client
  use_range_0 = true
else
  local s, e = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
  if s then
    s = tonumber(s); e = (e and e ~= "") and tonumber(e) or nil
    if s == 0 and (not e) then
      use_range_0 = true
    elseif s > 0 and s <= RANGE_TEE_THRESHOLD then
      near_start, near_end = s, e
      use_range_0 = true
    else
      -- Far-seek: stream without cache; follow redirects; never expose Location
      return stream_no_cache(orig, false)
    end
  else
    -- Malformed Range: treat as no-range
    use_range_0 = true
  end
end
-- Only one writer per key
local added = inflight:add(key, true, 900)
if not added then
  -- Writer exists; attempt follower if client requested a range slice
  if req_range and req_range ~= "" then
    local s, e = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
    if s then
      local err = serve_from_part(orig, key, tonumber(s), (e and e ~= "") and tonumber(e) or nil)
      if not err then return end
      if VERBOSE then ngx.log(ngx.INFO,"[streamcache] follow failed: ", tostring(err), " ; fallback nocache key=", key) end
    end
  end
  -- Fallback: stream nocache (never leak redirects)
  return stream_no_cache(orig, (not req_range or req_range == ""))
end

-- Start tee now
if near_start then
  return tee_stream_range_window(orig, final_path, key, near_start, near_end, orig)
else
  return tee_stream(orig, final_path, key, use_range_0, orig)
end
