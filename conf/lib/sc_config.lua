-- sc_config.lua
-- Configuration loader from environment variables.

local Config = {}

local function parse_bytes(str)
  if str == nil then return nil end
  if type(str) ~= "string" then str = tostring(str) end
  str = str:match("^%s*(.-)%s*$")
  if str == "" then return nil end

  local n, unit = str:match("^([%+%-]?%d+%.?%d*)%s*([bBkKmMgGtTpP][bB]?)$")
  if n then
    local multipliers = {
      b  = 1,
      k  = 1024,       kb = 1024,
      m  = 1024 ^ 2,   mb = 1024 ^ 2,
      g  = 1024 ^ 3,   gb = 1024 ^ 3,
      t  = 1024 ^ 4,   tb = 1024 ^ 4,
      p  = 1024 ^ 5,   pb = 1024 ^ 5,
    }
    local mult = multipliers[unit:lower()]
    if not mult then return tonumber(str) end  -- unrecognized suffix → raw fallback
    return tonumber(n) * mult
  end

  return tonumber(str)
end

-- Parse human-readable duration strings.
--   Accepted forms: "12h", "30m", "7200s", "7d" (suffix-only, no compounds)
--   Bare number    : interpreted as seconds (e.g., "3600" → 3600)
--   Returns nil for empty/unparseable input.
local function parse_duration(str)
  if str == nil then return nil end
  if type(str) ~= "string" then str = tostring(str) end
  str = str:match("^%s*(.-)%s*$")
  if str == "" then return nil end

  local n, unit = str:match("^(%d+%.?%d*)%s*([sSmMhHdD])$")
  if n then
    local multipliers = {
      s = 1,
      m = 60,
      h = 3600,
      d = 86400,
    }
    local mult = multipliers[unit:lower()]
    if not mult then return tonumber(str) end
    return tonumber(n) * mult
  end

  -- Bare number = seconds
  return tonumber(str)
end

function Config.load()
  local os = os

  local conf = {
    VERBOSE                  = (os.getenv("LOG_VERBOSE") == "1"),
    RANGE_TEE_THRESHOLD      = parse_bytes(os.getenv("RANGE_TEE_THRESHOLD")   or "") or (5 * 1024 * 1024),
    PROGRESS_FLUSH_BYTES     = parse_bytes(os.getenv("PROGRESS_FLUSH_BYTES")  or "") or 262144,
    FOLLOWER_WAIT_MAX        = tonumber(os.getenv("FOLLOWER_WAIT_MAX")     or "") or 30,
    FOLLOWER_POLL_MS         = tonumber(os.getenv("FOLLOWER_POLL_MS")      or "") or 50,
    FOLLOWER_START_WAIT_MS   = tonumber(os.getenv("FOLLOWER_START_WAIT_MS")or "") or 750,
    FOLLOWER_MIN_BYTES       = parse_bytes(os.getenv("FOLLOWER_MIN_BYTES")    or "") or 65536,
    CLIENT_FLUSH_BYTES       = parse_bytes(os.getenv("CLIENT_FLUSH_BYTES")    or "") or (1 * 1024 * 1024),
    TOTALS_TTL_SECS          = tonumber(os.getenv("TOTALS_TTL_SECS")       or "") or 86400,
    -- Cache revalidation: minimum elapsed time since last successful validation
    -- before the next stale check fires. Accepts s|m|h|d suffix or bare seconds.
    CACHE_REVALIDATE_INTERVAL = parse_duration(os.getenv("CACHE_REVALIDATE_INTERVAL") or "") or (12 * 3600),
    -- Upstream HTTP client read timeout in milliseconds.
    -- Applies to streaming reads from the provider; 0 = no timeout (legacy behavior).
    UPSTREAM_READ_TIMEOUT_MS = tonumber(os.getenv("UPSTREAM_READ_TIMEOUT_MS") or "") or 30000,
    SSL_VERIFY               = (os.getenv("DISABLE_SSL_VERIFY") ~= "1"),
    ALLOWED_HOSTS            = os.getenv("ALLOWED_HOSTS"),
    CACHE_DIR                = "/var/cache/streamcache/files",
    TMP_DIR                  = "/var/cache/streamcache/tmp",
    META_DIR                 = "/var/cache/streamcache/meta",
    -- Inflight TTL: how long the writer lock lasts before expiry (seconds).
    -- Must exceed the longest expected download time.
    INFLIGHT_TTL             = 900,
    -- Fadvise threshold: writer drops page cache pages every N bytes.
    -- Smaller = less dirty page accumulation but more syscalls.
    FADVISE_CHUNK            = 2 * 1024 * 1024,
  }
  return conf
end

Config.parse_bytes    = parse_bytes
Config.parse_duration = parse_duration

return Config
