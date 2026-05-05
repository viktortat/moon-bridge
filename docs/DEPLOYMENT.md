# Deployment

MoonBridge supports two deployment targets: standalone binary and Cloudflare Workers WASM.

## Standalone Binary

### Build

```bash
# Current platform
go build -o moonbridge ./cmd/moonbridge

# Cross-platform
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o moonbridge-linux-amd64 ./cmd/moonbridge
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o moonbridge-linux-arm64 ./cmd/moonbridge
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o moonbridge-darwin-arm64 ./cmd/moonbridge
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o moonbridge-windows-amd64.exe ./cmd/moonbridge
```

### Docker

```bash
docker build -t moonbridge .
docker run -d \
  --name moonbridge \
  -p 38440:38440 \
  -v "$PWD/config.yml:/config/config.yml" \
  moonbridge
```

The Dockerfile uses multi-stage build: `golang:1.26-bookworm` builder → `gcr.io/distroless/static-debian12:nonroot` runtime.

### Systemd

```ini
[Unit]
Description=MoonBridge
After=network.target

[Service]
ExecStart=/usr/local/bin/moonbridge -config /etc/moonbridge/config.yml
Restart=always
User=moonbridge

[Install]
WantedBy=multi-user.target
```

## Cloudflare Workers

```bash
# Requires pnpm
pnpm install
pnpm run deploy
```

Configuration is injected via `MOONBRIDGE_CONFIG` Wrangler secret. D1 database binding is optional (for persistence).

## GitHub Actions CI

The project includes `.github/workflows/ci.yml` that:
1. Runs `go test ./...` on every push
2. Builds for 6 platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64)
3. Creates a draft GitHub release with attached binaries on version tags (`v*`)

## Requirements

- No external dependencies (SQLite is embedded pure-Go)
- No CGO required for any build target
- Minimum ~10MB disk for the binary
- Outbound HTTPS access to upstream LLM providers
