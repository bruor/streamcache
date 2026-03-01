-- sc_config.lua
-- Configuration loader from environment variables.

local Config = {}

function Config.load()
  local os = os
  local tonumber = tonumber

  local conf = {
    VERBOSE               = (os.getenv("LOG_VERBOSE") == "1"),
    RANGE_TEE_THRESHOLD   = tonumber(os.getenv("RANGE_TEE_THRESHOLD")   or "") or (5 * 1024 * 1024),
    PROGRESS_FLUSH_BYTES  = tonumber(os.getenv("PROGRESS_FLUSH_BYTES")  or "") or 262144,
    FOLLOWER_WAIT_MAX     = tonumber(os.getenv("FOLLOWER_WAIT_MAX")     or "") or 600,
    FOLLOWER_POLL_MS      = tonumber(os.getenv("FOLLOWER_POLL_MS")      or "") or 50,
    FOLLOWER_START_WAIT_MS= tonumber(os.getenv("FOLLOWER_START_WAIT_MS")or "") or 750,
    FOLLOWER_MIN_BYTES    = tonumber(os.getenv("FOLLOWER_MIN_BYTES")    or "") or 65536,
    CLIENT_FLUSH_BYTES    = tonumber(os.getenv("CLIENT_FLUSH_BYTES")    or "") or (1 * 1024 * 1024),
    TOTALS_TTL_SECS       = tonumber(os.getenv("TOTALS_TTL_SECS")       or "") or 86400,
    SSL_VERIFY            = (os.getenv("DISABLE_SSL_VERIFY") ~= "1"),
    ALLOWED_HOSTS         = os.getenv("ALLOWED_HOSTS"),
    CACHE_DIR             = "/var/cache/streamcache/files",
    TMP_DIR               = "/var/cache/streamcache/tmp",
    META_DIR              = "/var/cache/streamcache/meta",
    -- Inflight TTL: how long the writer lock lasts before expiry (seconds).
    -- Must exceed the longest expected download time.
    INFLIGHT_TTL          = 900,
    -- Fadvise threshold: writer drops page cache pages every N bytes.
    -- Smaller = less dirty page accumulation but more syscalls.
    FADVISE_CHUNK         = 2 * 1024 * 1024,
  }
  return conf
end

return Config
