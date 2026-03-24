# ============================================
# Stage 1: Build
# ============================================
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build HTTP server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /bin/http-server ./apps/api/cmd/http-server

# ============================================
# Stage 2: HTTP Server (production)
# ============================================
FROM alpine:3.21 AS http-server

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S app && adduser -S app -G app

COPY --from=builder /bin/http-server /usr/local/bin/http-server
COPY --from=builder /src/apps/api/db/migrations /app/apps/api/db/migrations

RUN chown -R app:app /app

WORKDIR /app

USER app

EXPOSE 3001

ENTRYPOINT ["http-server"]
