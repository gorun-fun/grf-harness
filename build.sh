#!/bin/sh
# build.sh 一键：vet + 编译（产物 gh，跳过测试）
set -e
cd "$(dirname "$0")"
GO=${GO:-go}
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
    GO=/usr/local/go/bin/go
fi
echo "== vet =="
"$GO" vet ./...
echo "== build =="
"$GO" build -trimpath -ldflags "-s -w" -o gh .
# macOS：链接器的 adhoc 签名在文件被复制/Finder 拷贝后可能被系统
# 直接 SIGKILL（zsh: killed）；重新 adhoc 签名一次即可规避
if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
    codesign -f -s - gh >/dev/null 2>&1 || true
fi
echo "OK: ./gh version"
./gh version
