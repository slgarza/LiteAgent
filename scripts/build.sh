#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
LITEAGENT_GO_BIN="${LITEAGENT_GO_BIN:-go}"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$LITEAGENT_GO_BIN" build -trimpath -ldflags='-s -w' -o dist/liteagent ./cmd/liteagent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$LITEAGENT_GO_BIN" build -trimpath -tags desktop -ldflags='-s -w -H=windowsgui' -o dist/LiteAgent.exe ./cmd/liteagent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 "$LITEAGENT_GO_BIN" build -trimpath -ldflags='-s -w' -o dist/LiteAgent-console.exe ./cmd/liteagent
cp "$("$LITEAGENT_GO_BIN" env GOROOT)/LICENSE" dist/GO-LICENSE.txt
cp scripts/Start-LiteAgent.bat dist/Start-LiteAgent.bat
python3 scripts/package.py
