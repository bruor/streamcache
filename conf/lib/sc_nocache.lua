-- sc_nocache.lua
-- No-cache streaming: far-seek passthrough and HEAD handling.
-- These code paths do NOT write to disk — they stream directly from
-- upstream to the client, following redirects and masking Location headers.

local ngx      = ngx
local Utils    = require "sc_utils"
local Upstream = require "sc_upstream"

local NoCache = {}

-- Stream upstream response to client without caching.
-- Follows redirects internally (via Upstream.request); never exposes
-- Location headers to the client.
--
-- normalize_200: if true, converts 206→200 when client didn't send Range
--   (used when the proxy added Range:0- internally)
function NoCache.stream(url, cfg, client_headers, normalize_200)
  Utils.set_var_safe("sc_decision",
    (ngx.var.sc_decision ~= "" and ngx.var.sc_decision) or "MISS_NO_CACHE")

  local ch = client_headers or ngx.req.get_headers()
  local headers = Upstream.build_client_like_headers(nil, url, ch)
  headers["Range"] = ch["Range"] or ch["range"]  -- passthrough range as-is

  local res, httpc, _ = Upstream.request(url, {
    method          = "GET",
    headers         = headers,
    ssl_verify      = cfg.SSL_VERIFY,
    read_timeout_ms = cfg.UPSTREAM_READ_TIMEOUT_MS,
  })
  if not res then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  -- Single-shot upstream cleanup. Idempotent so on_abort + end-of-function
  -- can both call it safely.
  local httpc_closed = false
  local function close_upstream()
    if not httpc_closed then
      httpc_closed = true
      pcall(function() if httpc then httpc:close() end end)
    end
  end

  -- Register abort detection BEFORE we start consuming upstream bytes.
  -- When the client disconnects mid-stream, ngx.on_abort fires immediately
  -- (TCP RST/FIN) instead of waiting for the kernel send buffer to fill
  -- before pcall(ngx.print) returns false. Closing httpc here also stops
  -- the upstream from continuing to push data we'd just discard.
  local aborted = false
  pcall(ngx.on_abort, function()
    aborted = true
    close_upstream()
  end)

  -- Parse total size for normalization
  local hl = {}
  for k, v in pairs(res.headers or {}) do hl[string.lower(k)] = v end
  local total_size = nil
  if hl["content-range"] then
    local t0 = hl["content-range"]:match("/(%d+)$")
    if t0 then total_size = tonumber(t0) end
  end
  if not total_size and hl["content-length"] and res.status == 200 then
    total_size = tonumber(hl["content-length"])
  end

  -- Filter headers (consistent hop-by-hop stripping)
  local hcopy = Utils.filter_response_headers(res.headers)
  hcopy["X-Cache"] = "MISS"

  -- Normalize 206→200 when client didn't request a Range
  local client_sent_range = (ch["Range"] or ch["range"]) ~= nil
  if normalize_200 and (not client_sent_range) and res.status == 206 then
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

  -- Stream body with batched flushes
  local flushed = 0
  if res.body_reader then
    while true do
      if aborted then break end
      local chunk, cerr = res.body_reader(32768)
      if cerr then
        Utils.log_event(ngx.WARN, "NOCACHE_ERR", "upstream read error: " .. tostring(cerr))
        close_upstream()
        return ngx.exit(ngx.HTTP_BAD_GATEWAY)
      end
      if not chunk then break end
      local okp = pcall(ngx.print, chunk)
      if not okp then
        -- Print failed: client gone (or connection broken). Stop reading
        -- upstream immediately to avoid accumulating Lua strings we'd just
        -- discard during rapid-seek scenarios.
        close_upstream()
        break
      end
      flushed = flushed + #chunk
      if flushed >= cfg.CLIENT_FLUSH_BYTES then
        local okf = pcall(ngx.flush, true)
        if not okf then close_upstream(); break end
        flushed = 0
      end
    end
  elseif res.body then
    local okp = pcall(ngx.print, res.body)
    if okp then pcall(ngx.flush, true) end
  end

  pcall(ngx.flush, true)
  close_upstream()
  if aborted and cfg.VERBOSE then
    Utils.log_event(ngx.INFO, "DISCONNECT", "nocache stream aborted by client")
  end
  return
end

-- HEAD handler: follow redirects; never expose Location; no body.
-- Also handles 405 fallback (GET bytes=0-0) for servers that reject HEAD.
function NoCache.head(url, cfg, client_headers)
  Utils.set_var_safe("sc_decision", "HEAD_NOCACHE")
  if cfg.VERBOSE then
    Utils.log_event(ngx.INFO, "HEAD", "url: " .. tostring(url))
  end

  local ch = client_headers or ngx.req.get_headers()
  local headers = Upstream.build_client_like_headers(nil, url, ch)

  -- Try HEAD first, fall back to GET 0-0
  local res, httpc, _ = Upstream.request(url, {
    method     = "HEAD",
    headers    = headers,
    ssl_verify = cfg.SSL_VERIFY,
  })
  if not res then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end

  if res.status == 405 then
    httpc:close()
    headers["Range"] = headers["Range"] or "bytes=0-0"
    res, httpc, _ = Upstream.request(url, {
      method     = "GET",
      headers    = headers,
      ssl_verify = cfg.SSL_VERIFY,
    })
    if not res then return ngx.exit(ngx.HTTP_BAD_GATEWAY) end
  end

  -- Filter headers
  local hcopy = Utils.filter_response_headers(res.headers)
  ngx.status = ngx.HTTP_OK
  for k, v in pairs(hcopy) do ngx.header[k] = v end
  ngx.send_headers()
  httpc:close()
  return
end

return NoCache
