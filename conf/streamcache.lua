-- streamcache.lua
-- Entry point: routing logic only.
-- Single-pass tee + redirect-follow; never leak Location/30x to clients.
--
-- Request flow:
--   1. Decode/validate URL
--   2. Allowed hosts check
--   3. Non-GET → head passthrough
--   4. Cache HIT → serve from disk (with optional revalidation probe)
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
    inflight       = ngx.shared.inflight,
    access         = ngx.shared.access,
    resolved       = ngx.shared.resolved,
    progress       = ngx.shared.progress,
    totals         = ngx.shared.totals,
    probe_methods  = ngx.shared.probe_methods,
  },
}

-- ======================== URL Decode & Validate ========================
local b64 = ngx.var.b64
local orig = b64 and Utils.b64url_decode(b64) or nil
if not orig or not orig:match("^https?://") then
  Utils.log_event(ngx.WARN, "BAD_REQUEST", "invalid URL")
  return ngx.exit(ngx.HTTP_BAD_REQUEST)
end
orig = orig:gsub("^%s+", ""):gsub("[%s\r\n]+$", "")

ctx.url = orig
ctx.rid = ngx.var.request_id or tostring(ngx.now())
ctx.key = Utils.key_from_url(orig)

local req_range_early = ngx.req.get_headers()["Range"]

-- ======================== Allowed Hosts ========================
local allowed_hosts = cfg.ALLOWED_HOSTS or ""
if allowed_hosts ~= "" then
  local source_url = orig or ""
  local host = source_url:match("^https?://([^/]+)") or ""
  local allowed = false
  for h in allowed_hosts:gmatch("[^,%s]+") do
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
    local revalidate_interval = cfg.CACHE_REVALIDATE_INTERVAL or (12 * 3600)
    local serve_from_cache = true
    local meta_path = cfg.META_DIR .. "/" .. key .. ".meta"
    local meta = File.read_meta(key, cfg.META_DIR) or {}

    -- Determine baseline timestamp for "when did we last verify origin?"
    -- Prefer last_validated (set by commit and by successful revalidations).
    -- Fall back to last_access from meta, then file mtime, then 0.
    local baseline = meta.last_validated or meta.last_access
    if not baseline then
      local h = io.popen('stat -c %Y "' .. final_path .. '" 2>/dev/null')
      if h then local t = h:read("*l"); h:close(); baseline = tonumber(t) end
    end
    if not baseline then baseline = 0 end
    local age = ngx.now() - baseline

    if revalidate_interval > 0 and age > revalidate_interval
       and not ctx.shared.inflight:get(key) then
      -- Revalidation due. Probe origin using the per-host method cache.
      local probe_hdrs = Upstream.build_client_like_headers(nil, orig)
      local probe = Upstream.probe_origin(orig, probe_hdrs, cfg.SSL_VERIFY,
                                          ctx.shared.probe_methods)

      if not probe.ok then
        -- Graceful fallback: serve stale from cache when origin unreachable.
        -- Don't update last_validated so the next request retries the probe
        -- promptly once the provider recovers.
        Utils.log_event(ngx.WARN, "REVALIDATE_FAIL",
          "probe failed; serving stale from cache (url: " .. orig .. ")")
      else
        -- Compare against stored origin signals.
        -- PRIMARY: size. Only invalidate when we have a positive answer
        -- AND it disagrees with what we cached.
        local stored_size  = meta.content_length or sz
        local size_changed = (probe.size and probe.size > 0
                              and stored_size and probe.size ~= stored_size)

        -- SECONDARY: ETag and Last-Modified, only if BOTH stored and current
        -- have the value. Missing-on-either-side ⇒ skip that signal so a
        -- provider stripping these headers doesn't trigger mass eviction.
        local etag_changed = (meta.etag and probe.etag
                              and meta.etag ~= probe.etag)
        local lm_changed   = (meta.last_modified and probe.last_modified
                              and meta.last_modified ~= probe.last_modified)

        if size_changed or etag_changed or lm_changed then
          local reason
          if size_changed then
            reason = "origin size " .. Utils.b2human(probe.size)
                  .. " ≠ cached " .. Utils.b2human(stored_size)
          elseif etag_changed then
            reason = "origin ETag changed"
          else
            reason = "origin Last-Modified changed"
          end
          Utils.log_request(ngx.NOTICE, {
            label    = "REVALIDATE_EVICT",
            url      = orig,
            path     = ngx.var.uri,
            range    = req_range_early or "none",
            decision = "REVALIDATE_EVICT",
            reason   = reason .. ", re-downloading",
          })
          File.remove(final_path)
          File.remove(meta_path)
          ctx.shared.inflight:delete(key)
          ctx.shared.totals:delete(key)
          serve_from_cache = false
        else
          -- Origin matches; bump last_validated so we don't re-probe
          -- until the next interval elapses.
          File.update_meta(key, { last_validated = ngx.now() }, cfg.META_DIR)
        end
      end
    end

    if serve_from_cache then
      Utils.log_request(ngx.NOTICE, {
        label    = "REQUEST",
        url      = orig,
        path     = ngx.var.uri,
        range    = req_range_early or "none",
        decision = "HIT",
        reason   = "file cached, size " .. Utils.b2human(sz),
      })
      local ts = ngx.now()
      ctx.shared.access:set(key, ts)
      -- Update only last_access; preserve last_validated, content_type, etag,
      -- last_modified. (update_meta does a read-merge-write.)
      File.update_meta(key, { last_access = ts }, cfg.META_DIR)
      return File.serve_cached(final_path, key, cfg)
    end
  end
end

-- ======================== Parse Range ========================
local req_range    = ngx.req.get_headers()["Range"]
local use_range_0  = false
local near_start, near_end = nil, nil

if not req_range or req_range == "" then
  use_range_0 = true
else
  local s, e = req_range:match("^bytes=(%d+)%-([%d%*]*)$")
  if s then
    s = tonumber(s)
    e = (e and e ~= "") and tonumber(e) or nil
    if s == 0 and (not e) then
      use_range_0 = true
    elseif s > 0 and s <= cfg.RANGE_TEE_THRESHOLD then
      near_start, near_end = s, e
      use_range_0 = true
      Utils.log_request(ngx.NOTICE, {
        label    = "REQUEST",
        url      = orig,
        path     = ngx.var.uri,
        range    = req_range,
        decision = "TEE_WINDOW",
        reason   = "range start " .. tostring(s) .. " ≤ threshold " .. tostring(cfg.RANGE_TEE_THRESHOLD) .. ", caching full file",
      })
    else
      Utils.log_request(ngx.NOTICE, {
        label    = "REQUEST",
        url      = orig,
        path     = ngx.var.uri,
        range    = req_range,
        decision = "NOCACHE",
        reason   = "range start " .. tostring(s) .. " > threshold " .. tostring(cfg.RANGE_TEE_THRESHOLD),
      })
      Utils.set_var_safe("sc_decision", "MISS_NO_CACHE")
      return NoCache.stream(orig, cfg, nil, false)
    end
  else
    use_range_0 = true
  end
end

-- ======================== Writer Election ========================
-- Capture client headers BEFORE timer (timer context can't access ngx.req)
local captured_headers = ngx.req.get_headers()

local added = ctx.shared.inflight:add(key, true, cfg.INFLIGHT_TTL)
if not added then
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
    Utils.log_request(ngx.NOTICE, {
      label    = "REQUEST",
      url      = orig,
      path     = ngx.var.uri,
      range    = req_range or "none",
      decision = "FOLLOW",
      reason   = "writer in progress",
    })
    local ok = Follower.try_follow_with_wait(ctx, orig, s, e)
    if ok then return end
    if cfg.VERBOSE then
      Utils.log_event(ngx.INFO, "FOLLOW_MISS", "not ready, fallback to nocache")
    end
  end
  Utils.set_var_safe("sc_decision", "MISS_NO_CACHE")
  Utils.log_request(ngx.NOTICE, {
    label    = "REQUEST",
    url      = orig,
    path     = ngx.var.uri,
    range    = req_range or "none",
    decision = "NOCACHE",
    reason   = "follow not ready, fallback",
  })
  return NoCache.stream(orig, cfg, nil, (not req_range or req_range == ""))
end

-- ======================== We Are Writer ========================
Utils.log_request(ngx.NOTICE, {
  label    = "REQUEST",
  url      = orig,
  path     = ngx.var.uri,
  range    = req_range or "none",
  decision = "TEE",
  reason   = "new writer, background download starting",
})

-- MISS-time probe-method discovery:
-- For unknown providers, run discovery synchronously now. This populates
-- the probe_methods cache so that the first stale check (and any future
-- probes) can use the correct method without re-discovering. Discovery
-- failures are logged at ERROR by probe_origin but DO NOT block writer
-- launch — the writer's full GET request will succeed independently in
-- many cases (e.g., providers that reject HEAD AND GET 0-0 but accept
-- full GET with redirect-following).
do
  local probe_hdrs = Upstream.build_client_like_headers(nil, orig, captured_headers)
  Upstream.ensure_method_known(orig, probe_hdrs, cfg.SSL_VERIFY,
                               ctx.shared.probe_methods)
end

local function start_writer_timer(range0)
  return ngx.timer.at(0, function(premature)
    if premature then return end
    Tee.tee_stream(ctx, orig, final_path, range0, captured_headers, false)
  end)
end

local ok_timer, err_timer
if near_start then
  ok_timer, err_timer = start_writer_timer(true)
else
  ok_timer, err_timer = start_writer_timer(use_range_0)
end

if not ok_timer then
  Utils.log_event(ngx.WARN, "TIMER_FAIL", "using inline tee: " .. tostring(err_timer))
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
