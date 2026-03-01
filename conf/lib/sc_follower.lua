-- sc_follower.lua
-- Follower logic: serves a client from a growing .part file written
-- by the tee writer.
--
-- IMPROVEMENTS over production:
--   - Reads cached origin headers from `resolved` shared dict (no extra HEAD)
--   - Reads .part via FFI fd + fadvise(DONTNEED) to prevent re-caching
--     pages the writer already dropped from page cache

local ngx      = ngx
local math     = math
local Utils    = require "sc_utils"
local File     = require "sc_file"
local Upstream = require "sc_upstream"

local Follower = {}

-- Read cached origin headers from the resolved shared dict.
-- Returns: content_type, etag, last_modified, content_disposition (all may be nil)
local function get_cached_headers(resolved, key)
  if not resolved then return nil end
  local hdr_str = resolved:get("hdr:" .. key)
  if not hdr_str then return nil end
  local ct, et, lm, cd = hdr_str:match("^(.-)\n(.-)\n(.-)\n(.*)$")
  if ct == "" then ct = nil end
  if et == "" then et = nil end
  if lm == "" then lm = nil end
  if cd == "" then cd = nil end
  return ct, et, lm, cd
end

-- Serve bytes from a growing .part file (or completed cache file).
-- Returns: true (success), or nil + error string
function Follower.serve(ctx, final_url, req_start, req_end)
  local key      = ctx.key
  local cfg      = ctx.config
  local progress = ctx.shared.progress
  local totals   = ctx.shared.totals
  local resolved = ctx.shared.resolved

  local final_path = cfg.CACHE_DIR .. "/" .. key

  -- Fast check: did it finish while we were waiting?
  if File.exists(final_path) then
    ngx.header["X-Cache"] = "HIT"
    Utils.set_var_safe("sc_decision", "HIT")
    return ngx.exec("/cache/" .. key)
  end

  -- Open .part via FFI (gives us fd for fadvise after reads)
  local tmp = cfg.TMP_DIR .. "/" .. key .. ".part"
  local fd = File.open_read_fd(tmp)
  if not fd then return nil, "no_part" end

  -- Wait for total size to become available (writer sets it early)
  local total_size = totals:get(key)
  local waited = 0
  while not total_size and waited < 2000 do
    ngx.sleep(0.05)
    waited = waited + 50
    total_size = totals:get(key)
  end
  if not total_size then File.close_fd(fd); return nil, "no_total" end
  total_size = tonumber(total_size)

  -- Compute range bounds
  local start = tonumber(req_start) or 0
  local stop  = (req_end and req_end ~= "") and tonumber(req_end) or (total_size - 1)
  if stop > (total_size - 1) then stop = total_size - 1 end
  if start > stop or start > total_size - 1 then
    File.close_fd(fd)
    ngx.header["Content-Range"] = string.format("bytes */%d", total_size)
    return ngx.exit(ngx.HTTP_REQUESTED_RANGE_NOT_SATISFIABLE)
  end

  -- Get origin headers: try cached first, fall back to HEAD probe
  local ct, et, lm, cd = get_cached_headers(resolved, key)
  if not ct then
    local oh = Upstream.fetch_origin_headers(final_url,
      Upstream.build_client_like_headers(nil, final_url), cfg.SSL_VERIFY)
    ct = oh["content-type"]
    et = oh["etag"]
    lm = oh["last-modified"]
    cd = oh["content-disposition"]
  end

  -- Determine whether client actually sent a Range header.
  -- If not, respond with 200 OK (not 206) to satisfy browsers like Chrome.
  local client_range = ngx.req.get_headers()["Range"]
  local client_sent_range = (client_range ~= nil and client_range ~= "")

  ngx.header["X-Cache"]       = "FOLLOW"
  Utils.set_var_safe("sc_decision", "FOLLOW")
  ngx.header["Accept-Ranges"] = "bytes"
  if ct then ngx.header["Content-Type"]        = ct end
  if et then ngx.header["ETag"]                = et end
  if lm then ngx.header["Last-Modified"]       = lm end
  if cd then ngx.header["Content-Disposition"]  = cd end

  if client_sent_range then
    -- Client asked for a range: send 206 with Content-Range
    ngx.status = ngx.HTTP_PARTIAL_CONTENT
    ngx.header["Content-Range"]  = string.format("bytes %d-%d/%d", start, stop, total_size)
    ngx.header["Content-Length"] = tostring(stop - start + 1)
  else
    -- Client sent no Range: send 200 with full Content-Length
    ngx.status = ngx.HTTP_OK
    ngx.header["Content-Length"] = tostring(total_size)
  end
  ngx.send_headers()

  -- Register abort handler: when the client disconnects, ngx.on_abort
  -- fires in a separate light thread and sets our flag. This works
  -- because lua_check_client_abort is on for this location.
  local aborted = false
  local ok_abort = pcall(ngx.on_abort, function() aborted = true end)

  -- Streaming loop: read from .part file as writer produces data
  local deadline = ngx.now() + cfg.FOLLOWER_WAIT_MAX
  local poll_sec = cfg.FOLLOWER_POLL_MS / 1000

  -- Wait for minimum bytes before starting to stream
  while (progress:get(key) or 0) < cfg.FOLLOWER_MIN_BYTES
        and ngx.now() < deadline and not aborted do
    ngx.sleep(poll_sec)
  end

  local sent = 0
  local needed = stop - start + 1
  local current_offset = start
  local last_drop = start  -- track fadvise position for read pages
  local FADVISE_READ_CHUNK = 2 * 1024 * 1024  -- drop read pages every 2MB

  File.seek_fd(fd, start)

  while sent < needed and not aborted do
    local p = progress:get(key) or 0
    local avail = p - current_offset
    if avail < 0 then avail = 0 end
    local remaining = needed - sent
    if avail > remaining then avail = remaining end

    if avail > 0 then
      local chunk_sz = math.min(avail, 8192)
      local chunk = File.read_fd(fd, chunk_sz)
      if not chunk then
        -- File read returned nothing; writer may not have flushed yet
        ngx.sleep(poll_sec)
      else
        -- Send to client; detect disconnect via both print and flush
        local okp = pcall(ngx.print, chunk)
        if not okp then break end
        local okf = pcall(ngx.flush, true)
        if not okf then break end

        sent = sent + #chunk
        current_offset = current_offset + #chunk

        -- Drop read pages from cache periodically (read-only: no sync needed)
        if (current_offset - last_drop) >= FADVISE_READ_CHUNK then
          File.drop_read_cache(fd, last_drop, current_offset - last_drop)
          last_drop = current_offset
        end
      end
    else
      -- No new data available; check abort and deadline
      if aborted then break end
      if ngx.now() > deadline then
        if current_offset > last_drop then
          File.drop_read_cache(fd, last_drop, current_offset - last_drop)
        end
        File.close_fd(fd)
        ngx.log(ngx.WARN, "[streamcache] follower timeout key=", key,
          " offset=", tostring(current_offset))
        return ngx.exit(ngx.HTTP_OK)
      end
      ngx.sleep(poll_sec)
    end
  end

  -- Cleanup: drop any remaining read pages and close fd
  if current_offset > last_drop then
    File.drop_read_cache(fd, last_drop, current_offset - last_drop)
  end
  File.close_fd(fd)
  return true
end

-- Brief wait-and-follow to avoid duplicate no-cache origin pulls.
function Follower.try_follow_with_wait(ctx, final_url, start, stop)
  local cfg = ctx.config
  local deadline = ngx.now() + (cfg.FOLLOWER_START_WAIT_MS / 1000)

  while true do
    local ok, err = Follower.serve(ctx, final_url, start, stop)
    if ok then return true end
    if ngx.now() >= deadline then return false, err end
    if err ~= "no_part" and err ~= "no_total" then return false, err end
    ngx.sleep(0.05)
  end
end

return Follower
