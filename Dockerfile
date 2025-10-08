# ~/streamcache/Dockerfile
FROM openresty/openresty:alpine

# OPM needs perl and curl; CA certs for HTTPS
RUN apk add --no-cache perl curl ca-certificates && update-ca-certificates

# Install lua-resty-http via OPM
RUN /usr/local/openresty/bin/opm get ledgetech/lua-resty-http
