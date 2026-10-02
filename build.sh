#!/usr/bin/env bash
# 构建 codex-policy 插件，产物输出到 dist/
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out="$root/dist"
mkdir -p "$out"

cd "$root/codex-policy"
go build -buildmode=c-shared -o "$out/codex-policy.so" .

echo "built: $out/codex-policy.so"
sha256sum "$out/codex-policy.so"
