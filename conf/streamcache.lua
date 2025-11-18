-- streamcache.lua
-- Single-pass tee + redirect-follow; never leak Location/30x to clients.

local ngx  = ngx
local os   = os
local http = require "resty.http"



-- Safe setter for nginx vars (no-ops outside request phases)
local function set_var_safe(name, value)
  local phase = ngx.get_phase()
  if phase == "rewrite" or phase == "access" or phase == "content" or phase == "log" then
    pcall(function() ngx.var[name] = value end)
  end
end

-- ===================== Config (via env) =====================
local VERBOSE               = (os.getenv("LOG_VERBOSE") == "1")
local RANGE_TEE_THRESHOLD   = tonumber(os.getenv("RANGE_TEE_THRESHOLD")   or "") or (5 * 1024 * 1024) -- 5 MiB
local PROGRESS_FLUSH_BYTES  = tonumber(os.getenv("PROGRESS_FLUSH_BYTES")  or "") or 262144            -- 256 KiB
local FOLLOWER_WAIT_MAX     = tonumber(os.getenv("FOLLOWER_WAIT_MAX")     or "") or 600              -- seconds
local FOLLOWER_POLL_MS      = tonumber(os.getenv("FOLLOWER_POLL_MS")      or "") or 50               -- ms
-- New: short startup wait for followers so we don't open a duplicate upstream
local FOLLOWER_START_WAIT_MS= tonumber(os.getenv("FOLLOWER_START_WAIT_MS")or "") or 750              -- ms
-- New: batch client flushes to reduce syscall and allocator pressure
local CLIENT_FLUSH_BYTES    = tonumber(os.getenv("CLIENT_FLUSH_BYTES")    or "") or (1 * 1024 * 1024) -- 1 MiB
-- New: how long to retain the known TOTAL size (refresh while downloading)
local TOTALS_TTL_SECS       = tonumber(os.getenv("TOTALS_TTL_SECS")       or "") or 86400            -- 24h
local SSL_VERIFY            = (os.getenv("DISABLE_SSL_VERIFY") ~= "1")

-- Correlation id and structured logging helper
local RID = ngx.var.request_id or tostring(ngx.now())
local function jlog(level, event, fields)
  if not VERBOSE then return end
  fields = fields or {}
  fields.rid = RID
  local buf = {}
  for k,v in pairs(fields) do
    buf[#buf+1] = string.format('"%s":"%s"', k, tostring(v))
  end
  ngx.log(level, "[streamcache] ", event, " {", table.concat(buf, ","), "}")
end

-- ===================== Helpers =====================
local function b2human(n)
  if not n or n < 0 then return "0 B" end
  local u = {"B","KB","MB","GB","TB","PB"}; local i=1
  while n >= 1024 and i < #u do n = n/1024; i = i+1 end
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

local function file_exists(p)
  local f=io.open(p,"rb"); if f then f:close(); return true end; return false
end

local function file_size(p)
  local f=io.open(p,"rb"); if not f then return 0 end; local sz=f:seek("end"); f:close(); return sz or 0
end

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
  if loc:match("^//")       then return scheme .. ":" .. loc end
  if loc:match("^[^/]+:%d+/.") or loc:match("^[^/]+/") then return scheme .. "://" .. loc end
  if loc:sub(1,1) == "/"   then return scheme .. "://" .. host .. loc end
  local base = current:match("^(https?://[^?]+)")
  local dir  = base and base:match("(.*/)") or (scheme .. "://" .. host .. "/")
  return dir .. loc
end

local function tclone(t) local o={}; if not t then return o end; for k,v in pairs(t) do o[k]=v end; return o end

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
local inflight = ngx.shared.inflight
local access   = ngx.shared.access
local resolved = ngx.shared.resolved -- kept for future hints
local progress = ngx.shared.progress
local totals   = ngx.shared.totals

-- Refresh helper: keep the expected total alive during long/throttled downloads
local function refresh_totals(key, size)
  if not key or not size or size <= 0 then return end
  totals:set(key, size, TOTALS_TTL_SECS) -- set() refreshes TTL
end

-- ===================== No-cache streamer (far-seek passthrough & HEAD) =====================
local function stream_no_cache(url, normalize_200_when_no_client_range)
  set_var_safe("sc_decision", (ngx.var.sc_decision ~= "" and ngx.var.sc_decision) or "MISS_NO_CACHE")
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
  if hl["content-range"] then
    local s0,e0,t0 = hl["content-range"]:match("^bytes%s+(%d+)%-(%d+)/(%d+)$")
    if t0 then total_size = tonumber(t0) end
  end
  if not total_size and hl["content-length"] and res.status == 200 then
    total_size = tonumber(hl["content-length"])
  end

  local hcopy = {}
  for k,v in pairs(res.headers or {}) do
    local kl = string.lower(k)
    if kl ~= "transfer-encoding" and kl ~= "connection" and kl ~= "keep-alive"
       and kl ~= "proxy-authenticate" and kl ~= "proxy-authorization"
       and kl ~= "te" and kl ~= "trailer" and kl ~= "upgrade"
       and kl ~= "location" then -- never expose redirects
      hcopy[k] = v
    end
  end
  hcopy["X-Cache"] = "MISS"
  hcopy["Accept-Ranges"] = "bytes"

  if normalize_200_when_no_client_range and (not client_sent_range) and res.status == 206 then
    hcopy["Content-Range"] = nil
    if total_size and total_size > 0 then
      hcopy["Content-Length"] = tostring(total_size)
    else
      hcopy["Content-Length"] = nil
    end
    ngx.status = ngx.HTTP_OK
  else
    ngx.status = res.status
  end

  for k,v in pairs(hcopy) do ngx.header[k]=v end
  ngx.send_headers()

  -- Batch client flushes
  local flushed = 0
  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] nocache read error: ", tostring(cerr)); httpc:close(); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      local okp = pcall(ngx.print, chunk)
      if okp then
        flushed = flushed + #chunk
        if flushed >= CLIENT_FLUSH_BYTES then ngx.flush(true); flushed = 0 end
      end
    end
  elseif res.body then
    local okp = pcall(ngx.print, res.body); if okp then ngx.flush(true) end
  end
  -- Final flush
  ngx.flush(true)
  httpc:close()
  return
end

-- HEAD helper: follow redirects; never expose Location; no body
local function head_no_cache(url)
  set_var_safe("sc_decision", "HEAD_NOCACHE")
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

-- ===================== Tee: upstream -> client + file (strict commit) =====================
-- send_to_client = true (default): original behavior (sets headers & streams to client)
-- send_to_client = false        : writer-only mode (no client headers/prints); just writes .part & updates progress/totals
local function tee_stream(final_url, dest_path, key, use_range_0, orig_url_for_ref, send_to_client)
  if send_to_client == nil then send_to_client = true end
  set_var_safe("sc_decision", "MISS_TEE")
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

  local headers
  if send_to_client then
    headers = build_client_like_headers(host_header, orig_url_for_ref)
    local ch = ngx.req.get_headers()
    if ch["Range"] and ch["Range"] ~= "" then
      headers["Range"] = ch["Range"]
    elseif use_range_0 then
      headers["Range"] = "bytes=0-"
    else
      headers["Range"] = nil
    end
  else
    -- writer-only: decide Range solely by policy, not by request headers
    headers = { ["Host"] = host_header, ["User-Agent"] = "EmbyStreamCache/1.0" }
    if use_range_0 then headers["Range"] = "bytes=0-" end
  end

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

  -- Parse totals (size hint)
  local hl = {}; for k,v in pairs(res.headers or {}) do hl[string.lower(k)] = v end
  local total_size = nil
  if hl["content-range"] then
    local s0,e0,t0 = hl["content-range"]:match("^bytes%s+(%d+)%-(%d+)/(%d+)$")
    if t0 then total_size = tonumber(t0) end
  end
  if not total_size and hl["content-length"] and res.status == 200 then
    total_size = tonumber(hl["content-length"])
  end
  if total_size and total_size > 0 then refresh_totals(key, total_size) end

  -- Prepare response headers to client (only if send_to_client); normalize 206->200 if no client Range
  if send_to_client then
    local ch = ngx.req.get_headers()
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
      if total_size and total_size > 0 then
        hcopy["Content-Length"] = tostring(total_size)
      else
        hcopy["Content-Length"] = nil
      end
      ngx.status = ngx.HTTP_OK
    else
      ngx.status = res.status
    end
    for k,v in pairs(hcopy) do ngx.header[k] = v end
    ngx.send_headers()
  end

  -- Open temp file & tee (with no-cache fallback)
  os.execute("mkdir -p /var/cache/streamcache/tmp")
  local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"
  os.remove(tmp) -- best-effort cleanup from prior crashes
  local f = io.open(tmp, "wb")
  if not f then
    ngx.log(ngx.ERR,"[streamcache] tee cannot open temp: ", tmp, " ; falling back")
    httpc:close(); inflight:delete(key); return
  end

  local total = 0
  local last_report = 0
  local client_alive = send_to_client
  local flushed = 0

  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] tee read error: ",tostring(cerr)); f:close(); os.remove(tmp); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      total = total + #chunk
      local okw, errw = f:write(chunk)
      if not okw then
        ngx.log(ngx.ERR,"[streamcache] write failed: ", tostring(errw), " rid=", RID)
        f:close(); os.remove(tmp); httpc:close(); inflight:delete(key)
        return ngx.exit(ngx.HTTP_BAD_GATEWAY)
      end
      if (total - last_report) >= PROGRESS_FLUSH_BYTES then
        local okf, errf = pcall(function() return f:flush() end)
        if not okf then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf)) end
        progress:set(key, total, 900); last_report = total
        local exp = totals:get(key); if exp then refresh_totals(key, tonumber(exp)) end
      end
      if client_alive then
        local okp = pcall(ngx.print, chunk)
        if not okp then
          client_alive=false
        else
          flushed = flushed + #chunk
          if flushed >= CLIENT_FLUSH_BYTES then ngx.flush(true); flushed = 0 end
        end
      end
    end
  elseif res.body then
    total = #res.body
    local okw, errw = f:write(res.body)
    if not okw then
      ngx.log(ngx.ERR,"[streamcache] write failed: ", tostring(errw), " rid=", RID)
      f:close(); os.remove(tmp); httpc:close(); inflight:delete(key)
      return ngx.exit(ngx.HTTP_BAD_GATEWAY)
    end
    local okf, errf = pcall(function() return f:flush() end)
    if not okf then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf)) end
    progress:set(key, total, 900)
    local exp = totals:get(key); if exp then refresh_totals(key, tonumber(exp)) end
    if client_alive then pcall(ngx.print, res.body); ngx.flush(true) end
  end
  if client_alive then ngx.flush(true) end

  local okf2, errf2 = pcall(function() return f:flush() end)
  if not okf2 then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf2)) end
  progress:set(key, total, 900)
  local exp2 = totals:get(key); if exp2 then refresh_totals(key, tonumber(exp2)) end
  f:close(); httpc:close()

  -- Strict commit: only if expected size is known and fully matched
  local expected = tonumber(totals:get(key) or 0)
  if expected > 0 then
    if total == expected then
      local okr, errr = os.rename(tmp, dest_path)
      if not okr then
        ngx.log(ngx.ERR,"[streamcache] commit rename failed: ", tostring(errr),
        " tmp=", tmp, " dest=", dest_path, " rid=", RID)
        -- Best-effort copy fallback then remove tmp
        local src = io.open(tmp, "rb"); local dst = io.open(dest_path, "wb")
        if src and dst then
          local buf = src:read("*a"); if buf then dst:write(buf) end
          dst:close(); src:close(); os.remove(tmp)
        end
      end
      local ts = ngx.now(); write_meta(key, total, ts); access:set(key, ts)
      jlog(ngx.NOTICE, "tee_complete", {size=b2human(total), expected=expected})
    else
      os.remove(tmp)
      ngx.log(ngx.WARN,"[streamcache] not committing (truncated): written=", tostring(total),
      " expected=", tostring(expected), " key=", key)
    end
  else
    -- Unknown expected size => do not commit to cache
    if total > 0 then
      os.remove(tmp)
      ngx.log(ngx.WARN,"[streamcache] not committing (unknown total size); key=", key, " written=", tostring(total))
    else
      os.remove(tmp); ngx.log(ngx.WARN,"[streamcache] tee produced 0 bytes; not committing key=", key)
    end
  end
  inflight:delete(key)
  return
end

-- Tee with near-start window: fetch 0-; cache full; serve client slice only (strict commit)
-- (now also supports send_to_client=false, though we do not use it for writer)
local function tee_stream_range_window(final_url, dest_path, key, client_start, client_end, orig_url_for_ref, send_to_client)
  if send_to_client == nil then send_to_client = true end
  set_var_safe("sc_decision", "MISS_TEE_WINDOW")
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
    local s0,e0,t0 = headers_l["content-range"]:match("^bytes%s+(%d+)%-(%d+)/(%d+)$")
    if s0 and tonumber(s0) == 0 and e0 and t0 then total_size = tonumber(t0) end
  end
  if not total_size and headers_l["content-length"] and res.status == 200 then
    total_size = tonumber(headers_l["content-length"])
  end
  if total_size and total_size > 0 then refresh_totals(key, total_size) end

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

  if send_to_client then
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
  end

  -- Open temp & stream windowed slice while caching full body (with no-cache fallback)
  os.execute("mkdir -p /var/cache/streamcache/tmp")
  local tmp = "/var/cache/streamcache/tmp/" .. key .. ".part"
  os.remove(tmp)
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
      if send_to_client and #slice > 0 then pcall(ngx.print, slice) end -- batched below
      return #slice
    end
    local flushed_nc = 0
    if res.body_reader then
      while true do
        local chunk, cerr = res.body_reader(32768)
        if cerr then ngx.log(ngx.WARN,"[streamcache] read error (window nocache fallback): ", tostring(cerr)); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
        if not chunk then break end
        maybe_send_slice(chunk, pos)
        pos = pos + #chunk
        if send_to_client then
          flushed_nc = flushed_nc + #chunk
          if flushed_nc >= CLIENT_FLUSH_BYTES then ngx.flush(true); flushed_nc = 0 end
        end
      end
    elseif res.body then
      maybe_send_slice(res.body, pos); if send_to_client then ngx.flush(true) end
    end
    if send_to_client then ngx.flush(true) end
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
    if send_to_client and #slice > 0 then pcall(ngx.print, slice) end -- no per-slice flush; batched below
    return #slice
  end

  local flushed2 = 0
  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then ngx.log(ngx.WARN,"[streamcache] tee-window read error: ",tostring(cerr)); f:close(); os.remove(tmp); httpc:close(); inflight:delete(key); return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
      if not chunk then break end
      total_written = total_written + #chunk
      local okw, errw = f:write(chunk)
      if not okw then
        ngx.log(ngx.ERR,"[streamcache] write failed: ", tostring(errw), " rid=", RID)
        f:close(); os.remove(tmp); httpc:close(); inflight:delete(key)
        return ngx.exit(ngx.HTTP_BAD_GATEWAY)
      end
      if (total_written - last_report) >= PROGRESS_FLUSH_BYTES then
        local okf, errf = pcall(function() return f:flush() end)
        if not okf then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf)) end
        progress:set(key, total_written, 900); last_report = total_written
        local exp = totals:get(key); if exp then refresh_totals(key, tonumber(exp)) end
      end
      sent = sent + maybe_send_slice(chunk, pos)
      pos  = pos  + #chunk

      if send_to_client then
        flushed2 = flushed2 + #chunk
        if flushed2 >= CLIENT_FLUSH_BYTES then ngx.flush(true); flushed2 = 0 end
      end
    end
  elseif res.body then
    local chunk = res.body
    total_written = #chunk
    local okw, errw = f:write(chunk)
    if not okw then
      ngx.log(ngx.ERR,"[streamcache] write failed: ", tostring(errw), " rid=", RID)
      f:close(); os.remove(tmp); httpc:close(); inflight:delete(key)
      return ngx.exit(ngx.HTTP_BAD_GATEWAY)
    end
    local okf, errf = pcall(function() return f:flush() end)
    if not okf then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf)) end
    progress:set(key, total_written, 900)
    local exp = totals:get(key); if exp then refresh_totals(key, tonumber(exp)) end
    sent = sent + maybe_send_slice(chunk, pos)
    pos  = pos  + #chunk
  end
  if send_to_client then ngx.flush(true) end

  local okf2, errf2 = pcall(function() return f:flush() end)
  if not okf2 then ngx.log(ngx.WARN,"[streamcache] flush failed: ", tostring(errf2)) end
  progress:set(key, total_written, 900)
  local exp2 = totals:get(key); if exp2 then refresh_totals(key, tonumber(exp2)) end
  f:close(); httpc:close()

  -- Commit strictly only if complete
  local expected = tonumber(totals:get(key) or 0)
  if expected > 0 and total_written == expected then
    local okr, errr = os.rename(tmp, dest_path)
    if not okr then
      ngx.log(ngx.ERR,"[streamcache] commit rename failed: ", tostring(errr),
      " tmp=", tmp, " dest=", dest_path, " rid=", RID)
      local src = io.open(tmp, "rb"); local dst = io.open(dest_path, "wb")
      if src and dst then
        local buf = src:read("*a"); if buf then dst:write(buf) end
        dst:close(); src:close(); os.remove(tmp)
      end
    end
    local ts = ngx.now(); write_meta(key, total_written, ts); access:set(key, ts)
    jlog(ngx.NOTICE, "tee_window_complete", {
      size=b2human(total_written),
      served=tostring(sent),
      start=tostring(send_start),
      stop=tostring(send_end)
    })
  else
    os.remove(tmp)
    if expected > 0 then
      ngx.log(ngx.WARN,"[streamcache] not committing (truncated): written=", tostring(total_written),
      " expected=", tostring(expected), " key=", key)
    else
      ngx.log(ngx.WARN,"[streamcache] not committing (unknown total size); key=", key,
      " written=", tostring(total_written))
    end
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
    set_var_safe("sc_decision", "HIT")
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
  set_var_safe("sc_decision", "FOLLOW")
  ngx.header["Accept-Ranges"]  = "bytes"
  ngx.header["Content-Range"]  = string.format("bytes %d-%d/%d", start, stop, total_size)
  ngx.header["Content-Length"] = tostring(stop - start + 1)
  if oh["content-type"]        then ngx.header["Content-Type"]        = oh["content-type"] end
  if oh["etag"]                then ngx.header["ETag"]               = oh["etag"]          end
  if oh["last-modified"]       then ngx.header["Last-Modified"]      = oh["last-modified"] end
  if oh["content-disposition"] then ngx.header["Content-Disposition"]= oh["content-disposition"] end
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

-- Helper: brief wait-and-follow to avoid duplicate no-cache origin pulls
local function try_follow_with_wait(orig, key, req_start, req_end)
  local deadline = ngx.now() + (FOLLOWER_START_WAIT_MS / 1000)
  while true do
    local err = serve_from_part(orig, key, req_start, req_end)
    if not err then return true end                         -- follow succeeded
    if ngx.now() >= deadline then return false, err end     -- give up after short wait
    if err ~= "no_part" and err ~= "no_total" then          -- non-retryable reason
      return false, err
    end
    ngx.sleep(0.05)
  end
end

-- ===================== Entry (HEAD-safe + policy) =====================

-- Decode Base64URL
local b64  = ngx.var.b64
local orig = b64 and b64url_decode(b64) or nil
if not orig or not orig:match("^https?://") then
  ngx.log(ngx.WARN,"[streamcache] bad request (invalid or non-http URL)")
  return ngx.exit(ngx.HTTP_BAD_REQUEST)
end
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
local key        = key_from_url(orig)
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
    ngx.var.sc_decision = "HIT"
    update_last_access()
    return ngx.exec("/cache/" .. key)
  end
end

-- Decide path based on client Range
local req_range   = ngx.req.get_headers()["Range"]
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
      ngx.var.sc_decision = "MISS_NO_CACHE"
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
  -- Writer exists. Prefer to FOLLOW from .part for near-start or no-range
  local s, e = nil, nil
  if req_range and req_range ~= "" then
    local s1, e1 = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
    if s1 then s = tonumber(s1); e = (e1 and e1 ~= "") and tonumber(e1) or nil end
  end
  if not s then s = 0; e = nil end
  if s == 0 or s <= RANGE_TEE_THRESHOLD then
    local ok = try_follow_with_wait(orig, key, s, e)
    if ok then return end
    if VERBOSE then ngx.log(ngx.INFO,"[streamcache] follow not ready; fallback nocache key=", key) end
  end
  -- Fallback: stream nocache (never leak redirects)
  set_var_safe("sc_decision", ((not req_range or req_range == "") and "MISS_NO_CACHE") or "MISS_NO_CACHE")
  return stream_no_cache(orig, (not req_range or req_range == ""))
end

-- We are the first request (writer). Start a background writer-only timer
local function start_writer_timer(range0)
  return ngx.timer.at(0, function(premature)
    if premature then return end
    -- Writer-only: decoupled from client speed
    tee_stream(orig, final_path, key, range0, orig, false)
  end)
end

local ok_timer, err_timer
-- For best reuse, prefer writing from the start when feasible
if near_start then
  ok_timer, err_timer = start_writer_timer(true)   -- force Range: 0-
else
  ok_timer, err_timer = start_writer_timer(use_range_0)
end

-- If the timer could not be started, fall back to original tee (client-coupled)
if not ok_timer then
  if VERBOSE then ngx.log(ngx.WARN, "[streamcache] writer timer failed: ", tostring(err_timer), " ; using inline tee") end
  if near_start then
    return tee_stream_range_window(orig, final_path, key, near_start, near_end, orig, true)
  else
    return tee_stream(orig, final_path, key, use_range_0, orig, true)
  end
end

-- Now follow from the .part for this client
local follow_start, follow_end = 0, nil
if req_range and req_range ~= "" then
  local s2, e2 = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
  if s2 then
    follow_start = tonumber(s2) or 0
    follow_end   = (e2 and e2 ~= "") and tonumber(e2) or nil
  end
end

local ok_follow = try_follow_with_wait(orig, key, follow_start, follow_end)
if ok_follow then
  return
end

-- Last-resort fallback (edge): stream nocache
return stream_no_cache(orig, (not req_range or req_range == ""))
