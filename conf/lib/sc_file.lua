-- sc_file.lua
-- File operations with LuaJIT FFI for posix_fadvise support.
-- Handles cache file creation, commit, and page-cache management.
--
-- Page cache strategy:
--   Writer: write() → sync_file_range(WRITE) → fadvise(DONTNEED)
--     fadvise only drops CLEAN pages; dirty pages are ignored. We must
--     flush dirty pages to disk first via sync_file_range, then drop.
--   Follower: read via FFI fd → fadvise(DONTNEED) on read data
--     Prevents follower reads from re-caching pages the writer dropped.

local ngx = ngx
local os  = os
local io  = io
local ffi = require "ffi"
local bit = require "bit"

ffi.cdef[[
  int open(const char *pathname, int flags, int mode);
  long read(int fd, void *buf, unsigned long count);
  long write(int fd, const void *buf, unsigned long count);
  int close(int fd);
  int fsync(int fd);
  long lseek(int fd, long offset, int whence);
  int posix_fadvise(int fd, long offset, long len, int advice);
  int sync_file_range(int fd, long offset, long nbytes, unsigned int flags);
  char *strerror(int errnum);
]]

-- Linux x86_64 constants
local O_RDONLY = 0
local O_WRONLY = 1
local O_CREAT  = 64
local O_TRUNC  = 512
local O_WRITE_FLAGS = bit.bor(O_WRONLY, O_CREAT, O_TRUNC)

local S_IRUSR  = 256
local S_IWUSR  = 128
local S_IRGRP  = 32
local S_IROTH  = 4
local S_MODE   = bit.bor(S_IRUSR, S_IWUSR, S_IRGRP, S_IROTH) -- 0644

local SEEK_SET = 0

local POSIX_FADV_DONTNEED = 4

-- sync_file_range flags
local SYNC_FILE_RANGE_WAIT_BEFORE = 1
local SYNC_FILE_RANGE_WRITE       = 2

local File = {}

function File.exists(path)
  local f = io.open(path, "rb")
  if f then f:close(); return true end
  return false
end

function File.size(path)
  local f = io.open(path, "rb")
  if not f then return 0 end
  local sz = f:seek("end")
  f:close()
  return sz or 0
end

function File.remove(path)
  if path and path ~= "" then
    pcall(os.remove, path)
  end
end

-- Write cache metadata as a full replacement.
-- meta is a table with any of these fields:
--   size            (number)  required-ish; defaults to 0
--   last_access     (number)  defaults to ngx.now()
--   last_validated  (number)  optional; epoch of last successful origin probe
--   content_length  (number)  optional; origin Content-Length at commit
--   content_type    (string)  optional; origin Content-Type at commit
--   etag            (string)  optional; origin ETag at commit
--   last_modified   (string)  optional; origin Last-Modified at commit
-- Backward-compatible: janitor's parse_meta only reads `size` and `last_access`,
-- so it ignores any extra fields.
function File.write_meta(key, meta, meta_dir)
  meta_dir = meta_dir or "/var/cache/streamcache/meta"
  local f = io.open(meta_dir .. "/" .. key .. ".meta", "wb")
  if not f then return end
  meta = meta or {}
  f:write("size=" .. tostring(meta.size or 0) .. "\n")
  f:write("last_access=" .. tostring(meta.last_access or ngx.now()) .. "\n")
  if meta.last_validated then
    f:write("last_validated=" .. tostring(meta.last_validated) .. "\n")
  end
  if meta.content_length then
    f:write("content_length=" .. tostring(meta.content_length) .. "\n")
  end
  if meta.content_type and meta.content_type ~= "" then
    f:write("content_type=" .. tostring(meta.content_type) .. "\n")
  end
  if meta.etag and meta.etag ~= "" then
    f:write("etag=" .. tostring(meta.etag) .. "\n")
  end
  if meta.last_modified and meta.last_modified ~= "" then
    f:write("last_modified=" .. tostring(meta.last_modified) .. "\n")
  end
  f:close()
end

-- Read cache metadata. Returns a table with present fields, or nil if no
-- meta file exists. Numeric fields are converted; string fields are kept as-is.
function File.read_meta(key, meta_dir)
  meta_dir = meta_dir or "/var/cache/streamcache/meta"
  local f = io.open(meta_dir .. "/" .. key .. ".meta", "rb")
  if not f then return nil end
  local meta = {}
  for line in f:lines() do
    local k, v = line:match("^([a-z_]+)=(.+)$")
    if k and v then
      if k == "size" or k == "last_access" or k == "last_validated"
         or k == "content_length" then
        meta[k] = tonumber(v)
      else
        meta[k] = v
      end
    end
  end
  f:close()
  return meta
end

-- Read existing meta, merge updates, write back. Lossless for fields not
-- mentioned in `updates`. Used to bump last_access or last_validated without
-- losing content_type/etag/last_modified.
function File.update_meta(key, updates, meta_dir)
  local existing = File.read_meta(key, meta_dir) or {}
  for k, v in pairs(updates or {}) do
    existing[k] = v
  end
  File.write_meta(key, existing, meta_dir)
end

-- Create directories. Accepts a table of paths or a single string.
function File.ensure_dirs(dirs)
  if type(dirs) == "table" then
    for _, d in ipairs(dirs) do
      os.execute("mkdir -p " .. d)
    end
  else
    os.execute("mkdir -p " .. tostring(dirs))
  end
end

-- Rename with chunked copy fallback + post-commit size verification.
function File.rename(src, dest, expected_size)
  local ok, err = os.rename(src, dest)
  if ok then
    if expected_size and expected_size > 0 then
      local actual = File.size(dest)
      if actual ~= expected_size then
        ngx.log(ngx.ERR, "[streamcache] post-rename size mismatch: expected=",
          tostring(expected_size), " actual=", tostring(actual))
        File.remove(dest)
        return false, "post_rename_size_mismatch"
      end
    end
    return true
  end

  -- Cross-filesystem fallback: chunked copy (1MB chunks)
  local src_f = io.open(src, "rb")
  local dst_f = io.open(dest, "wb")
  if not src_f or not dst_f then
    if src_f then src_f:close() end
    if dst_f then dst_f:close() end
    return false, err
  end

  local CHUNK_SZ = 1024 * 1024
  local copied = 0
  while true do
    local chunk = src_f:read(CHUNK_SZ)
    if not chunk then break end
    local wok, werr = dst_f:write(chunk)
    if not wok then
      dst_f:close(); src_f:close()
      File.remove(dest)
      return false, "copy_write_failed: " .. tostring(werr)
    end
    copied = copied + #chunk
  end
  dst_f:close()
  src_f:close()

  if expected_size and expected_size > 0 and copied ~= expected_size then
    ngx.log(ngx.ERR, "[streamcache] post-copy size mismatch: expected=",
      tostring(expected_size), " copied=", tostring(copied))
    File.remove(dest)
    return false, "post_copy_size_mismatch"
  end

  File.remove(src)
  return true
end

-- ======================== FFI: Write Operations ========================

function File.open_fd(path)
  local fd = ffi.C.open(path, O_WRITE_FLAGS, S_MODE)
  if fd < 0 then return nil, "open_error" end
  return fd
end

function File.write_fd(fd, data)
  local len = #data
  local written = 0
  local ptr = ffi.cast("const char*", data)
  while written < len do
    local res = ffi.C.write(fd, ptr + written, len - written)
    if res < 0 then return nil, "write_error" end
    written = written + tonumber(res)
  end
  return written
end

function File.close_fd(fd)
  if fd and fd >= 0 then ffi.C.close(fd) end
end

function File.fsync_fd(fd)
  if fd and fd >= 0 then ffi.C.fsync(fd) end
end

-- ======================== FFI: Read Operations (for follower) ========================

-- Reusable read buffer (avoids per-call allocation)
local READ_BUF_SIZE = 65536
local read_buf = ffi.new("char[?]", READ_BUF_SIZE)

function File.open_read_fd(path)
  local fd = ffi.C.open(path, O_RDONLY, 0)
  if fd < 0 then return nil, "open_error" end
  return fd
end

function File.seek_fd(fd, offset)
  if fd and fd >= 0 then
    local pos = ffi.C.lseek(fd, offset, SEEK_SET)
    return tonumber(pos)
  end
  return -1
end

function File.read_fd(fd, size)
  if not fd or fd < 0 then return nil end
  if size > READ_BUF_SIZE then size = READ_BUF_SIZE end
  local n = ffi.C.read(fd, read_buf, size)
  if n <= 0 then return nil end
  return ffi.string(read_buf, n)
end

function File.serve_cached(path, key, cfg)
  local total_size = File.size(path)
  if total_size <= 0 then return nil, "empty_file" end

  local req_range = ngx.req.get_headers()["Range"]
  local start_byte = 0
  local stop_byte  = total_size - 1
  local client_sent_range = (req_range ~= nil and req_range ~= "")

  if client_sent_range then
    local s, e = req_range:match("^bytes=(%d+)%-([%d]*)$")
    if s then
      start_byte = tonumber(s) or 0
      stop_byte  = (e and e ~= "") and tonumber(e) or (total_size - 1)
    end
    if stop_byte > (total_size - 1) then stop_byte = total_size - 1 end
    if start_byte >= total_size then
      ngx.header["Content-Range"] = string.format("bytes */%d", total_size)
      return ngx.exit(ngx.HTTP_REQUESTED_RANGE_NOT_SATISFIABLE)
    end
  end

  local send_len = stop_byte - start_byte + 1

  local fd = File.open_read_fd(path)
  if not fd then return nil, "open_error" end

  -- Read meta for origin headers (Content-Type, ETag, Last-Modified).
  -- Falls back to safe defaults when fields are absent (old meta files
  -- written before the extended format, or origin didn't send the header).
  local meta = (key and cfg and cfg.META_DIR) and File.read_meta(key, cfg.META_DIR) or nil

  ngx.header["X-Cache"]       = "HIT"
  ngx.header["Accept-Ranges"] = "bytes"
  ngx.header["Content-Type"]  = (meta and meta.content_type) or "application/octet-stream"
  if meta then
    if meta.etag          then ngx.header["ETag"]          = meta.etag end
    if meta.last_modified then ngx.header["Last-Modified"] = meta.last_modified end
  end

  if client_sent_range then
    ngx.status = ngx.HTTP_PARTIAL_CONTENT
    ngx.header["Content-Range"]  = string.format("bytes %d-%d/%d", start_byte, stop_byte, total_size)
    ngx.header["Content-Length"] = tostring(send_len)
  else
    ngx.status = ngx.HTTP_OK
    ngx.header["Content-Length"] = tostring(total_size)
  end
  ngx.send_headers()

  File.seek_fd(fd, start_byte)

  local FADVISE_CHUNK = 2 * 1024 * 1024
  local sent = 0
  local needed = send_len
  local cur_offset = start_byte
  local last_drop = start_byte
  local flush_acc = 0
  local CLIENT_FLUSH = (cfg and cfg.CLIENT_FLUSH_BYTES) or (1 * 1024 * 1024)

  while sent < needed do
    local chunk_sz = math.min(8192, needed - sent)
    local chunk = File.read_fd(fd, chunk_sz)
    if not chunk then break end

    local okp = pcall(ngx.print, chunk)
    if not okp then break end

    sent = sent + #chunk
    cur_offset = cur_offset + #chunk
    flush_acc = flush_acc + #chunk

    if flush_acc >= CLIENT_FLUSH then
      pcall(ngx.flush, true)
      flush_acc = 0
    end

    if (cur_offset - last_drop) >= FADVISE_CHUNK then
      File.drop_read_cache(fd, last_drop, cur_offset - last_drop)
      last_drop = cur_offset
    end
  end

  if cur_offset > last_drop then
    File.drop_read_cache(fd, last_drop, cur_offset - last_drop)
  end

  pcall(ngx.flush, true)
  File.close_fd(fd)
  return true
end

-- ======================== Page Cache Management ========================

-- Initiate async writeback of dirty pages in the given range.
-- This does NOT block — it starts I/O and returns immediately.
-- Call drop_cache() on a PREVIOUS range after this to drop clean pages.
function File.start_writeback(fd, offset, len)
  if fd and fd >= 0 and len > 0 then
    ffi.C.sync_file_range(fd, offset, len, SYNC_FILE_RANGE_WRITE)
  end
end

-- Wait for pages in range to be clean, then tell kernel to drop them.
-- The two-step (sync_file_range + fadvise) is required because
-- fadvise(DONTNEED) silently ignores dirty pages.
function File.drop_cache(fd, offset, len)
  if fd and fd >= 0 and len > 0 then
    -- Wait for any in-progress writeback to complete
    ffi.C.sync_file_range(fd, offset, len,
      bit.bor(SYNC_FILE_RANGE_WAIT_BEFORE, SYNC_FILE_RANGE_WRITE))
    -- Now pages are clean; drop them from cache
    ffi.C.posix_fadvise(fd, offset, len, POSIX_FADV_DONTNEED)
  end
end

-- Drop clean (read-only) pages from cache. No sync_file_range needed
-- because the reader never dirties pages. Cheaper than drop_cache().
-- Used by the follower which only reads from .part files.
function File.drop_read_cache(fd, offset, len)
  if fd and fd >= 0 and len > 0 then
    ffi.C.posix_fadvise(fd, offset, len, POSIX_FADV_DONTNEED)
  end
end

-- Legacy alias (used in final-fadvise paths)
function File.fadvise(fd, offset, len)
  File.drop_cache(fd, offset, len)
end

return File
