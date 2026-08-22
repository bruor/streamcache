# Stage 1: Build binary
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Copy go.mod and download dependencies
COPY go.mod ./
RUN go mod download

# Copy the rest of the source code and assets
COPY . .

# Compile static binary for Linux
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o streamcache .

# Stage 2: Final minimal runtime image
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/streamcache .

# Copy static assets (HTML dashboard)
COPY --from=builder /app/web ./web

# Expose default port
EXPOSE 8080

# Set standard environment variables (can be overridden in docker-compose)
ENV PORT=8080
ENV CACHE_DIR=/var/cache/streamcache/files
ENV TMP_DIR=/var/cache/streamcache/tmp

CMD ["./streamcache"]
