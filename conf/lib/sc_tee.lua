-- sc_tee.lua
-- Tee streaming: upstream → client + disk (strict commit).
--
-- Two modes:
--   tee_stream:              Full-body tee; optional client serving
--   tee_stream_range_window: Cache full body; serve client a byte slice
--
-- Improvements over production monolith:
--   • FFI writes with posix_fadvise(FADV_DONTNEED) to prevent OOM
--   • Inflight TTL refresh during long downloads
--   • Post-commit size verification
--   • Chunked copy fallback (no read("*a") OOM)
--   • Origin headers cached in shared dict for follower use
--   • Range-hostile fallback (retry without Range on 416/400/403)

local ngx      = ngx
local math     = math
local Utils    = require "sc_utils"
local File     = require "sc_file"
local Upstream = require "sc_upstream"

local Tee = {}

-- ======================== Internal Helpers ========================

-- Unified cleanup: close connections, remove temp, clear inflight.
-- Safe in both request and timer contexts.
local function cleanup_writer(httpc, key, tmp, inflight)
  pcall(function() if httpc then httpc:close() end end)
  if tmp and tmp ~= "" then File.remove(tmp) end
  if key and inflight then inflight:delete(key) end
end

-- Unified failure: cleanup + exit (request) or return (timer).
local function fail_exit(code, httpc, key, tmp, inflight, send_to_client)
  cleanup_writer(httpc, key, tmp, inflight)
  if send_to_client then
    return ngx.exit(code)
  end
  return
end

-- Refresh the totals TTL during long downloads.
local function refresh_totals(totals, key, size, ttl)
  if not key or not size or size <= 0 then return end
  totals:set(key, size, ttl)
end

-- Cache origin response headers in the `resolved` shared dict so
-- followers can read them without making an extra HEAD request.
local function cache_origin_headers(resolved, key, response_headers)
  if not resolved or not response_headers then return end
  local hl = {}
  for k, v in pairs(response_headers) do hl[string.lower(k)] = v end
  -- Store as newline-delimited: content-type\netag\nlast-modified\ncontent-disposition
  local ct = hl["content-type"] or ""
  local et = hl["etag"] or ""
  local lm = hl["last-modified"] or ""
  local cd = hl["content-disposition"] or ""
  resolved:set("hdr:" .. key, ct .. "\n" .. et .. "\n" .. lm .. "\n" .. cd, 3600)
end

-- Parse total size from response headers (Content-Range or Content-Length).
local function parse_total_size(res)
  local hl = {}
  for k, v in pairs(res.headers or {}) do hl[string.lower(k)] = v end
  local total = nil
  if hl["content-range"] then
    local t0 = hl["content-range"]:match("/(%d+)$")
    if t0 then total = tonumber(t0) end
  end
  if not total and hl["content-length"] and res.status == 200 then
    total = tonumber(hl["content-length"])
  end
  return total, hl
end

-- Try a range-hostile retry: drop Range header, probe size via HEAD first.
-- Returns new res, httpc or nil.
local function range_hostile_retry(url, options, totals, key, cfg, orig_url, client_headers)
  local probe_hdrs = Upstream.build_client_like_headers(nil, orig_url, client_headers)
  local expected_head, _ = Upstream.probe_total_with_head(url, probe_hdrs, cfg.SSL_VERIFY)
  if expected_head and expected_head > 0 then
    totals:set(key, expected_head, cfg.TOTALS_TTL_SECS)
  else
    totals:delete(key)
  end
  -- Retry without Range
  local retry_opts = Utils.tclone(options)
  retry_opts.headers = Utils.tclone(options.headers or {})
  retry_opts.headers["Range"] = nil
  return Upstream.request(url, retry_opts)
end

-- ======================== Streaming Write Loop ========================
-- Shared by both tee_stream and tee_stream_range_window.
-- Writes upstream body to an FFI file descriptor while optionally
-- sending bytes to the client (full body or windowed slice).
--
-- Returns: total_written, sent_to_client, error_string
local function write_loop(ctx, res, httpc, fd, tmp, send_to_client, client_opts)
  local cfg = ctx.config
  local key = ctx.key
  local rid = ctx.rid
  local progress = ctx.shared.progress
  local totals   = ctx.shared.totals
  local inflight = ctx.shared.inflight

  local total_written = 0
  local last_report   = 0
  local last_fadvise  = 0
  local prev_wb_off   = nil  -- previous writeback range offset
  local prev_wb_len   = nil  -- previous writeback range length
  local flushed       = 0
  local sent          = 0
  local client_alive  = send_to_client

  -- Windowed slice parameters (for tee_stream_range_window)
  local slice_start = client_opts and client_opts.start or 0
  local slice_end   = client_opts and client_opts.stop  -- nil = end of body

  -- How to decide what bytes to send to client
  local function maybe_send_slice(chunk, chunk_start)
    if not client_alive then return 0 end
    local chunk_len = #chunk
    local chunk_end_pos = chunk_start + chunk_len - 1

    -- If windowed mode
    if client_opts then
      local s_end = slice_end or (chunk_end_pos + 1)  -- default: no upper bound
      if chunk_end_pos < slice_start or chunk_start > s_end then return 0 end
      local from = math.max(slice_start - chunk_start, 0) + 1
      local to   = math.min(s_end - chunk_start + 1, chunk_len)
      local slice = chunk:sub(from, to)
      if #slice > 0 then
        local okp = pcall(ngx.print, slice)
        if not okp then client_alive = false; return 0 end
        return #slice
      end
      return 0
    else
      -- Full body mode
      local okp = pcall(ngx.print, chunk)
      if not okp then client_alive = false; return 0 end
      return #chunk
    end
  end

  local pos = 0 -- absolute position in upstream body

  if res.body_reader then
    while true do
      local chunk, cerr = res.body_reader(32768)
      if cerr then
        ngx.log(ngx.WARN, "[streamcache] tee read error: ", tostring(cerr))
        return 0, 0, "upstream_read_fail"
      end
      if not chunk then break end

      -- 1. Write to disk via FFI
      local written, errw = File.write_fd(fd, chunk)
      if not written or written ~= #chunk then
        ngx.log(ngx.ERR, "[streamcache] write failed: ", tostring(errw), " rid=", rid)
        return total_written, sent, "disk_write_fail"
      end
      total_written = total_written + #chunk

      -- 2. Progress + inflight TTL refresh
      if (total_written - last_report) >= cfg.PROGRESS_FLUSH_BYTES then
        progress:set(key, total_written, cfg.INFLIGHT_TTL)
        last_report = total_written
        -- Keep totals alive
        local exp = totals:get(key)
        if exp then refresh_totals(totals, key, tonumber(exp), cfg.TOTALS_TTL_SECS) end
        -- Refresh inflight lock so it doesn't expire during long downloads
        inflight:set(key, true, cfg.INFLIGHT_TTL)
      end

      -- 3. Page cache management: pipeline writeback + drop
      --    Step A: drop the PREVIOUS range (writeback was started last cycle)
      --    Step B: start async writeback of the CURRENT range
      --    This overlaps disk I/O with network I/O for zero extra latency.
      if (total_written - last_fadvise) >= cfg.FADVISE_CHUNK then
        -- Drop previous range (its writeback was started last iteration)
        if prev_wb_off then
          File.drop_cache(fd, prev_wb_off, prev_wb_len)
        end
        -- Start async writeback of current range
        local cur_off = last_fadvise
        local cur_len = total_written - last_fadvise
        File.start_writeback(fd, cur_off, cur_len)
        prev_wb_off = cur_off
        prev_wb_len = cur_len
        last_fadvise = total_written
      end

      -- 4. Send to client (full body or windowed slice)
      sent = sent + maybe_send_slice(chunk, pos)
      pos = pos + #chunk

      if client_alive then
        flushed = flushed + #chunk
        if flushed >= cfg.CLIENT_FLUSH_BYTES then ngx.flush(true); flushed = 0 end
      else
        -- No client: yield to event loop so kernel can flush dirty pages
        -- and other timers/connections get CPU time. Without this, the
        -- write loop runs at wire speed and starves everything.
        ngx.sleep(0)
      end
    end
  elseif res.body then
    local chunk = res.body
    total_written = #chunk
    local written, errw = File.write_fd(fd, chunk)
    if not written or written ~= #chunk then
      ngx.log(ngx.ERR, "[streamcache] write failed: ", tostring(errw), " rid=", rid)
      return total_written, sent, "disk_write_fail"
    end
    progress:set(key, total_written, cfg.INFLIGHT_TTL)
    local exp = totals:get(key)
    if exp then refresh_totals(totals, key, tonumber(exp), cfg.TOTALS_TTL_SECS) end
    inflight:set(key, true, cfg.INFLIGHT_TTL)
    sent = sent + maybe_send_slice(chunk, pos)
    pos = pos + #chunk
  end

  -- Final page cache cleanup: drop any remaining pages
  -- First, drop the previous writeback range if pending
  if prev_wb_off then
    File.drop_cache(fd, prev_wb_off, prev_wb_len)
  end
  -- Then flush + drop the tail
  if total_written > last_fadvise then
    File.drop_cache(fd, last_fadvise, total_written - last_fadvise)
  end

  -- Final progress update
  progress:set(key, total_written, cfg.INFLIGHT_TTL)

  if client_alive then ngx.flush(true) end

  return total_written, sent, nil
end

-- ======================== Strict Commit ========================
-- Only commits (renames .part → final) if written bytes exactly match
-- expected size. Uses chunked copy fallback + post-commit verification.
local function strict_commit(ctx, tmp, dest_path, total_written, rid)
  local totals  = ctx.shared.totals
  local access  = ctx.shared.access
  local cfg     = ctx.config
  local key     = ctx.key

  local expected = tonumber(totals:get(key) or 0)
  if expected > 0 then
    if total_written == expected then
      local okr, errr = File.rename(tmp, dest_path, expected)
      if not okr then
        ngx.log(ngx.ERR, "[streamcache] commit failed: ", tostring(errr),
          " tmp=", tmp, " dest=", dest_path, " rid=", rid)
        File.remove(tmp)
        return false, "commit_fail"
      end
      local ts = ngx.now()
      File.write_meta(key, total_written, ts, cfg.META_DIR)
      access:set(key, ts)
      if cfg.VERBOSE then
        Utils.jlog(ngx.NOTICE, "tee_complete",
          {size = Utils.b2human(total_written), expected = tostring(expected)}, rid, true)
      end
      return true, nil
    else
      File.remove(tmp)
      ngx.log(ngx.WARN, "[streamcache] not committing (truncated): written=",
        tostring(total_written), " expected=", tostring(expected), " key=", key)
      return false, "size_mismatch"
    end
  else
    File.remove(tmp)
    if total_written > 0 then
      ngx.log(ngx.WARN, "[streamcache] not committing (unknown total size); key=", key,
        " written=", tostring(total_written))
    else
      ngx.log(ngx.WARN, "[streamcache] tee produced 0 bytes; not committing key=", key)
    end
    return false, "unknown_size"
  end
end

-- ======================== tee_stream ========================
-- Full-body tee: upstream → client (optional) + disk.
-- send_to_client=true:  sets response headers and streams body to client
-- send_to_client=false: writer-only mode (background timer); no ngx.print
function Tee.tee_stream(ctx, url, dest_path, use_range_0, client_headers, send_to_client)
  if send_to_client == nil then send_to_client = true end
  Utils.set_var_safe("sc_decision", "MISS_TEE")

  local cfg = ctx.config
  local key = ctx.key
  local rid = ctx.rid
  local inflight = ctx.shared.inflight
  local totals   = ctx.shared.totals

  -- Build headers
  local ch = client_headers or ngx.req.get_headers()
  local headers
  if send_to_client then
    headers = Upstream.build_client_like_headers(nil, url, ch)
    local client_range = ch["Range"] or ch["range"]
    if client_range and client_range ~= "" then
      headers["Range"] = client_range
    elseif use_range_0 then
      headers["Range"] = "bytes=0-"
    end
  else
    -- Writer-only: use captured client headers for detection avoidance
    headers = Upstream.build_client_like_headers(nil, url, ch)
    if use_range_0 then headers["Range"] = "bytes=0-" end
    -- Don't forward client Range in writer mode — always fetch full file
    if not use_range_0 then headers["Range"] = nil end
  end

  local res, httpc, final_url = Upstream.request(url, {
    method     = "GET",
    headers    = headers,
    ssl_verify = cfg.SSL_VERIFY,
  })
  if not res then
    return fail_exit(ngx.HTTP_BAD_GATEWAY, nil, key, nil, inflight, send_to_client)
  end

  -- Range-hostile fallback: retry without Range on 416/400/403
  if not (res.status == 200 or res.status == 206) then
    if headers["Range"] and (res.status == 416 or res.status == 400 or res.status == 403) then
      httpc:close()
      local res2, httpc2, url2 = range_hostile_retry(
        url, {ssl_verify = cfg.SSL_VERIFY}, totals, key, cfg, url, ch)
      if res2 and (res2.status == 200 or res2.status == 206) then
        res, httpc, final_url = res2, httpc2, url2
      else
        if httpc2 then httpc2:close() end
        return fail_exit(ngx.HTTP_BAD_GATEWAY, nil, key, nil, inflight, send_to_client)
      end
    else
      ngx.log(ngx.WARN, "[streamcache] tee status not OK: ", res.status)
      return fail_exit(ngx.HTTP_BAD_GATEWAY, httpc, key, nil, inflight, send_to_client)
    end
  end

  -- Parse total size
  local total_size, hl = parse_total_size(res)
  if total_size and total_size > 0 then
    refresh_totals(totals, key, total_size, cfg.TOTALS_TTL_SECS)
  end

  -- Cache origin headers for followers
  cache_origin_headers(ctx.shared.resolved, key, res.headers)

  -- Send response headers to client
  if send_to_client then
    local client_sent_range = (ch["Range"] or ch["range"]) ~= nil
    local hcopy = Utils.filter_response_headers(res.headers)

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
    for k, v in pairs(hcopy) do ngx.header[k] = v end
    ngx.send_headers()
  end

  -- Open temp file via FFI
  File.ensure_dirs(cfg.TMP_DIR)
  local tmp = cfg.TMP_DIR .. "/" .. key .. ".part"
  File.remove(tmp)
  local fd, err_open = File.open_fd(tmp)
  if not fd then
    ngx.log(ngx.ERR, "[streamcache] tee cannot open temp (fd): ", tmp, " err=", tostring(err_open))
    return fail_exit(ngx.HTTP_BAD_GATEWAY, httpc, key, nil, inflight, send_to_client)
  end

  -- Stream & write
  local total_written, _, loop_err = write_loop(ctx, res, httpc, fd, tmp, send_to_client, nil)

  File.fsync_fd(fd)
  File.close_fd(fd)
  httpc:close()

  if loop_err then
    cleanup_writer(nil, key, tmp, inflight)
    return
  end

  -- Strict commit
  strict_commit(ctx, tmp, dest_path, total_written, rid)
  inflight:delete(key)
  return
end

-- ======================== tee_stream_range_window ========================
-- Cache full body from byte 0; serve client only the requested slice.
-- This optimizes IPTV playback startup: client gets its requested range
-- with low latency while the full file is cached in the background.
function Tee.tee_stream_range_window(ctx, url, dest_path, client_start, client_end,
                                      client_headers, send_to_client)
  if send_to_client == nil then send_to_client = true end
  Utils.set_var_safe("sc_decision", "MISS_TEE_WINDOW")

  local cfg = ctx.config
  local key = ctx.key
  local rid = ctx.rid
  local inflight = ctx.shared.inflight
  local totals   = ctx.shared.totals

  local ch = client_headers or ngx.req.get_headers()
  local headers = Upstream.build_client_like_headers(nil, url, ch)
  headers["Range"] = (ch["Range"] or ch["range"]) or "bytes=0-"

  local res, httpc, final_url = Upstream.request(url, {
    method     = "GET",
    headers    = headers,
    ssl_verify = cfg.SSL_VERIFY,
  })
  if not res then
    return fail_exit(ngx.HTTP_BAD_GATEWAY, nil, key, nil, inflight, send_to_client)
  end

  -- Range-hostile fallback
  if not (res.status == 200 or res.status == 206) then
    if headers["Range"] and (res.status == 416 or res.status == 400 or res.status == 403) then
      httpc:close()
      local res2, httpc2, url2 = range_hostile_retry(
        url, {ssl_verify = cfg.SSL_VERIFY}, totals, key, cfg, url, ch)
      if res2 and (res2.status == 200 or res2.status == 206) then
        res, httpc, final_url = res2, httpc2, url2
      else
        if httpc2 then httpc2:close() end
        return fail_exit(ngx.HTTP_BAD_GATEWAY, nil, key, nil, inflight, send_to_client)
      end
    else
      ngx.log(ngx.WARN, "[streamcache] tee-window status not OK: ", res.status)
      return fail_exit(ngx.HTTP_BAD_GATEWAY, httpc, key, nil, inflight, send_to_client)
    end
  end

  -- Parse total size
  local total_size, hl = parse_total_size(res)
  if total_size and total_size > 0 then
    refresh_totals(totals, key, total_size, cfg.TOTALS_TTL_SECS)
  end

  -- Cache origin headers for followers
  cache_origin_headers(ctx.shared.resolved, key, res.headers)

  -- If size unknown, fall back to nocache streaming
  if not total_size or total_size <= 0 then
    httpc:close()
    inflight:delete(key)
    local NoCache = require "sc_nocache"
    return NoCache.stream(url, cfg, ch, false)
  end

  -- Compute client slice bounds
  local send_start = math.max(0, tonumber(client_start) or 0)
  local send_end   = tonumber(client_end)
  if not send_end or send_end > (total_size - 1) then send_end = total_size - 1 end
  if send_start > send_end then
    httpc:close()
    inflight:delete(key)
    ngx.header["Content-Range"] = string.format("bytes */%d", total_size)
    return ngx.exit(ngx.HTTP_REQUESTED_RANGE_NOT_SATISFIABLE)
  end
  local send_len = send_end - send_start + 1

  -- Send 206 headers to client
  if send_to_client then
    local hcopy = {}
    for k, v in pairs(res.headers or {}) do
      local kl = string.lower(k)
      if kl == "content-type" or kl == "etag" or kl == "last-modified"
         or kl == "content-disposition" then
        hcopy[k] = v
      end
    end
    hcopy["Accept-Ranges"]  = "bytes"
    hcopy["Content-Range"]  = string.format("bytes %d-%d/%d", send_start, send_end, total_size)
    hcopy["Content-Length"]  = tostring(send_len)
    ngx.status = ngx.HTTP_PARTIAL_CONTENT
    for k, v in pairs(hcopy) do ngx.header[k] = v end
    ngx.send_headers()
  end

  -- Open temp file via FFI
  File.ensure_dirs(cfg.TMP_DIR)
  local tmp = cfg.TMP_DIR .. "/" .. key .. ".part"
  File.remove(tmp)
  local fd, err_open = File.open_fd(tmp)
  if not fd then
    ngx.log(ngx.ERR, "[streamcache] tee-window cannot open temp (fd): ", tmp,
      " err=", tostring(err_open), " ; falling back to nocache")
    httpc:close()
    inflight:delete(key)
    local NoCache = require "sc_nocache"
    return NoCache.stream(url, cfg, ch, false)
  end

  -- Stream & write with windowed client output
  local total_written, _, loop_err = write_loop(
    ctx, res, httpc, fd, tmp, send_to_client,
    { start = send_start, stop = send_end }
  )

  File.fsync_fd(fd)
  File.close_fd(fd)
  httpc:close()

  if loop_err then
    cleanup_writer(nil, key, tmp, inflight)
    return
  end

  -- Strict commit
  strict_commit(ctx, tmp, dest_path, total_written, rid)
  inflight:delete(key)
  return
end

return Tee
