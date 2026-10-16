# ---- build ----
# SQLite needs cgo, so build with a C toolchain. Alpine (musl) on both stages
# keeps the binaries compatible with the Litestream image below.
FROM golang:1.24-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
# Litestream continuously backs up the SQLite file to S3-compatible storage (e.g. Fly Tigris).
COPY --from=litestream/litestream:0.3.13 /usr/local/bin/litestream /usr/local/bin/litestream
COPY --from=build /out/server /app/server
COPY deploy/litestream.yml /etc/litestream.yml
COPY deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

ENV APP_ENV=production \
    DB_PATH=/data/app.db \
    PUBLIC_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/app/entrypoint.sh"]
