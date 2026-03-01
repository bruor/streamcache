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

-- Write cache metadata (size + last-access timestamp).
-- Uses standard io for small files; safe in any context.
function File.write_meta(key, size, last, meta_dir)
  meta_dir = meta_dir or "/var/cache/streamcache/meta"
  local f = io.open(meta_dir .. "/" .. key .. ".meta", "wb")
  if not f then return end
  f:write("size=" .. tostring(size or 0) .. "\n")
  f:write("last_access=" .. tostring(last or ngx.now()) .. "\n")
  f:close()
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
