#!/usr/bin/env bash
# build-release.sh —— 产出可分发的 wcpilot 包（exe + ocr_helper.py + 校验和）
#
#   ./scripts/build-release.sh 8.0.0
#
# 产物在 dist/ 下：
#   wcpilot.exe                     主程序
#   ocr_helper.py                   OCR 后端（可选依赖，放同目录即可用）
#   wcpilot-<ver>-windows-amd64.zip 分发包
#   SHA256SUMS.txt
set -euo pipefail

VER="${1:-dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo dev)"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
mkdir -p dist

echo "==> build wcpilot $VER (commit $COMMIT)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
  -trimpath \
  -ldflags "-s -w -X main.version=${VER} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}" \
  -o dist/wcpilot.exe .

cp ocr_helper.py dist/
cp README.md LICENSE CHANGELOG.md dist/ 2>/dev/null || true

echo "==> verify"
./dist/wcpilot.exe version

if command -v zip >/dev/null 2>&1; then
  (cd dist && zip -q -r "wcpilot-${VER}-windows-amd64.zip" wcpilot.exe ocr_helper.py README.md LICENSE CHANGELOG.md)
  echo "==> dist/wcpilot-${VER}-windows-amd64.zip"
fi

echo "==> sha256"
(cd dist && sha256sum wcpilot.exe ocr_helper.py > SHA256SUMS.txt && cat SHA256SUMS.txt)

ls -lh dist/
