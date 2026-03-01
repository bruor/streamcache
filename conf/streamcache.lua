-- streamcache.lua
-- Entry point: routing logic only.
-- Single-pass tee + redirect-follow; never leak Location/30x to clients.
--
-- Request flow:
--   1. Decode/validate URL
--   2. Allowed hosts check
--   3. Non-GET → head passthrough
--   4. Cache HIT → serve from disk
--   5. Parse Range header
--   6. Far seek (> threshold) → stream_no_cache
--   7. Writer election (inflight:add)
--      - Already inflight → follower
--      - New writer → background timer + client follows
--   8. Timer fallback → inline tee

local Config   = require "sc_config"
local Utils    = require "sc_utils"
local File     = require "sc_file"
local Upstream = require "sc_upstream"
local NoCache  = require "sc_nocache"
local Tee      = require "sc_tee"
local Follower = require "sc_follower"

local ngx = ngx

-- ======================== Bootstrap ========================
local cfg = Config.load()

local ctx = {
  config = cfg,
  shared = {
    inflight = ngx.shared.inflight,
    access   = ngx.shared.access,
    resolved = ngx.shared.resolved,
    progress = ngx.shared.progress,
    totals   = ngx.shared.totals,
  },
}

-- ======================== URL Decode & Validate ========================
local b64 = ngx.var.b64
local orig = b64 and Utils.b64url_decode(b64) or nil
if not orig or not orig:match("^https?://") then
  ngx.log(ngx.WARN, "[streamcache] bad request (invalid or non-http URL)")
  return ngx.exit(ngx.HTTP_BAD_REQUEST)
end
orig = orig:gsub("^%s+", ""):gsub("[%s\r\n]+$", "")

ctx.url = orig
ctx.rid = ngx.var.request_id or tostring(ngx.now())
ctx.key = Utils.key_from_url(orig)

-- ======================== Allowed Hosts ========================
if cfg.ALLOWED_HOSTS and cfg.ALLOWED_HOSTS ~= "" then
  local host = orig:match("^https?://([^/]+)")
  local allowed = false
  for h in cfg.ALLOWED_HOSTS:gmatch("[^,%s]+") do
    if host == h then allowed = true; break end
  end
  if not allowed then
    ngx.status = ngx.HTTP_FORBIDDEN
    ngx.say("Host not allowed")
    return
  end
end

-- ======================== Non-GET Methods ========================
local method = ngx.req.get_method()
if method ~= "GET" then
  return NoCache.head(orig, cfg)
end

-- ======================== Ensure Dirs ========================
File.ensure_dirs({cfg.CACHE_DIR, cfg.TMP_DIR, cfg.META_DIR})
local key = ctx.key
local final_path = cfg.CACHE_DIR .. "/" .. key

-- ======================== Cache HIT ========================
if File.exists(final_path) then
  local sz = File.size(final_path)
  if sz <= 0 then
    -- Corrupt zero-byte file: clean up
    File.remove(final_path)
    File.remove(cfg.META_DIR .. "/" .. key .. ".meta")
    ctx.shared.inflight:delete(key)
  else
    ngx.header["X-Cache"] = "HIT"
    Utils.set_var_safe("sc_decision", "HIT")
    if cfg.VERBOSE then
      ngx.log(ngx.INFO, "[streamcache] HIT key=", key, " size=", Utils.b2human(sz))
    end
    -- Update LRU
    local ts = ngx.now()
    ctx.shared.access:set(key, ts)
    File.write_meta(key, sz, ts, cfg.META_DIR)
    return ngx.exec("/cache/" .. key)
  end
end

-- ======================== Parse Range ========================
local req_range    = ngx.req.get_headers()["Range"]
local use_range_0  = false
local near_start, near_end = nil, nil

if not req_range or req_range == "" then
  -- No Range from client: behave like a player upstream (Range:0-)
  use_range_0 = true
else
  local s, e = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
  if s then
    s = tonumber(s)
    e = (e and e ~= "") and tonumber(e) or nil
    if s == 0 and (not e) then
      -- bytes=0- : same as no range
      use_range_0 = true
    elseif s > 0 and s <= cfg.RANGE_TEE_THRESHOLD then
      -- Near-start seek: tee full file, serve windowed slice
      near_start, near_end = s, e
      use_range_0 = true
    else
      -- Far-seek: stream without cache; follow redirects; never expose Location
      Utils.set_var_safe("sc_decision", "MISS_NO_CACHE")
      return NoCache.stream(orig, cfg, nil, false)
    end
  else
    -- Malformed Range: treat as no-range
    use_range_0 = true
  end
end

-- ======================== Writer Election ========================
-- Capture client headers BEFORE timer (timer context can't access ngx.req)
local captured_headers = ngx.req.get_headers()

local added = ctx.shared.inflight:add(key, true, cfg.INFLIGHT_TTL)
if not added then
  -- Writer exists: try to follow from .part
  local s, e = nil, nil
  if req_range and req_range ~= "" then
    local s1, e1 = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
    if s1 then
      s = tonumber(s1)
      e = (e1 and e1 ~= "") and tonumber(e1) or nil
    end
  end
  if not s then s = 0; e = nil end

  if s == 0 or s <= cfg.RANGE_TEE_THRESHOLD then
    local ok = Follower.try_follow_with_wait(ctx, orig, s, e)
    if ok then return end
    if cfg.VERBOSE then
      ngx.log(ngx.INFO, "[streamcache] follow not ready; fallback nocache key=", key)
    end
  end
  -- Fallback: stream nocache
  Utils.set_var_safe("sc_decision", "MISS_NO_CACHE")
  return NoCache.stream(orig, cfg, nil, (not req_range or req_range == ""))
end

-- ======================== We Are Writer ========================
-- Start background writer timer (decoupled from client speed).
-- Client becomes a follower reading from the growing .part file.

local function start_writer_timer(range0)
  return ngx.timer.at(0, function(premature)
    if premature then return end
    Tee.tee_stream(ctx, orig, final_path, range0, captured_headers, false)
  end)
end

local ok_timer, err_timer
if near_start then
  ok_timer, err_timer = start_writer_timer(true) -- force Range: 0-
else
  ok_timer, err_timer = start_writer_timer(use_range_0)
end

-- If timer could not start, fall back to inline tee (client-coupled)
if not ok_timer then
  if cfg.VERBOSE then
    ngx.log(ngx.WARN, "[streamcache] writer timer failed: ", tostring(err_timer),
      " ; using inline tee")
  end
  if near_start then
    return Tee.tee_stream_range_window(
      ctx, orig, final_path, near_start, near_end, captured_headers, true)
  else
    return Tee.tee_stream(ctx, orig, final_path, use_range_0, captured_headers, true)
  end
end

-- ======================== Client Follows Writer ========================
local follow_start, follow_end = 0, nil
if req_range and req_range ~= "" then
  local s2, e2 = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
  if s2 then
    follow_start = tonumber(s2) or 0
    follow_end   = (e2 and e2 ~= "") and tonumber(e2) or nil
  end
end

local ok_follow = Follower.try_follow_with_wait(ctx, orig, follow_start, follow_end)
if ok_follow then
  return
end

-- Last-resort fallback: stream nocache
return NoCache.stream(orig, cfg, nil, (not req_range or req_range == ""))
