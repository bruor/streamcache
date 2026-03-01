-- sc_utils.lua
-- Shared helpers used across all modules.

local ngx = ngx

local Utils = {}

function Utils.b2human(n)
  if not n or n < 0 then return "0 B" end
  local u = {"B","KB","MB","GB","TB","PB"}; local i = 1
  while n >= 1024 and i < #u do n = n / 1024; i = i + 1 end
  return string.format("%.2f %s", n, u[i])
end

-- Safe setter for nginx vars (no-ops outside request phases or timer context)
function Utils.set_var_safe(name, value)
  local phase = ngx.get_phase()
  if phase == "rewrite" or phase == "access" or phase == "content" or phase == "log" then
    pcall(function() ngx.var[name] = value end)
  end
end

-- Shallow clone
function Utils.tclone(t)
  local o = {}
  if not t then return o end
  for k, v in pairs(t) do o[k] = v end
  return o
end

-- Base64-URL decode (URL-safe variant with - and _)
function Utils.b64url_decode(s)
  s = s:gsub("-", "+"):gsub("_", "/")
  local pad = #s % 4
  if pad > 0 then s = s .. string.rep("=", 4 - pad) end
  return ngx.decode_base64(s)
end

function Utils.key_from_url(u)
  return ngx.md5(u)
end

-- Structured JSON-ish logging (only emits when verbose is true)
function Utils.jlog(level, event, fields, rid, verbose)
  if not verbose then return end
  fields = fields or {}
  fields.rid = rid
  local buf = {}
  for k, v in pairs(fields) do
    buf[#buf + 1] = string.format('"%s":"%s"', k, tostring(v))
  end
  ngx.log(level, "[streamcache] ", event, " {", table.concat(buf, ","), "}")
end

-- Standard hop-by-hop headers that must never be forwarded to clients.
-- Used by all response-sending code paths for consistent filtering.
Utils.HOP_BY_HOP = {
  ["transfer-encoding"]    = true,
  ["connection"]           = true,
  ["keep-alive"]           = true,
  ["proxy-authenticate"]   = true,
  ["proxy-authorization"]  = true,
  ["te"]                   = true,
  ["trailer"]              = true,
  ["upgrade"]              = true,
  ["location"]             = true,  -- never expose redirects to client
}

-- Filter response headers: strips hop-by-hop + adds cache metadata.
-- Returns a clean header table ready for ngx.header assignment.
function Utils.filter_response_headers(raw_headers)
  local out = {}
  for k, v in pairs(raw_headers or {}) do
    if not Utils.HOP_BY_HOP[string.lower(k)] then
      out[k] = v
    end
  end
  out["Accept-Ranges"] = "bytes"
  return out
end

return Utils
