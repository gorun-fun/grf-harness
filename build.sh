#!/bin/sh
# build.sh 一键：vet + 编译（跳过测试）
# 用法：./build.sh [目标]
#   （缺省）   当前系统，产物 ./gh
#   macos     darwin/arm64   → dist/gh-darwin-arm64
#   linux     linux/amd64    → dist/gh-linux-amd64
#   linux-arm linux/arm64    → dist/gh-linux-arm64
#   windows   windows/amd64  → dist/gh-windows-amd64.exe
#   all       上述四个平台全部
set -e
cd "$(dirname "$0")"
GO=${GO:-go}
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
    GO=/usr/local/go/bin/go
fi

# 交叉编译单个目标：参数为 名称 GOOS GOARCH 产物路径
# vet 也带 GOOS/GOARCH，可顺带检查平台专属文件（termios/term 回退等）
build_one() {
    echo "== vet ($1: $2/$3) =="
    GOOS=$2 GOARCH=$3 "$GO" vet ./...
    echo "== build ($1: $2/$3) =="
    GOOS=$2 GOARCH=$3 "$GO" build -trimpath -ldflags "-s -w" -o "$4" .
    # macOS：链接器的 adhoc 签名在文件被复制/Finder 拷贝后可能被系统
    # 直接 SIGKILL（zsh: killed）；重新 adhoc 签名一次即可规避
    # （非 macOS 主机没有 codesign，跳过）
    if [ "$2" = darwin ] && command -v codesign >/dev/null 2>&1; then
        codesign -f -s - "$4" >/dev/null 2>&1 || true
    fi
    echo "OK: $4"
}

case "${1:-}" in
    "")
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
        ;;
    macos)
        mkdir -p dist
        build_one macos darwin arm64 dist/gh-darwin-arm64
        ;;
    linux)
        mkdir -p dist
        build_one linux linux amd64 dist/gh-linux-amd64
        ;;
    linux-arm)
        mkdir -p dist
        build_one linux-arm linux arm64 dist/gh-linux-arm64
        ;;
    windows)
        mkdir -p dist
        build_one windows windows amd64 dist/gh-windows-amd64.exe
        ;;
    all)
        mkdir -p dist
        build_one macos darwin arm64 dist/gh-darwin-arm64
        build_one linux linux amd64 dist/gh-linux-amd64
        build_one linux-arm linux arm64 dist/gh-linux-arm64
        build_one windows windows amd64 dist/gh-windows-amd64.exe
        ;;
    *)
        echo "用法: $0 [macos|linux|linux-arm|windows|all]（缺省编译当前系统）" >&2
        exit 1
        ;;
esac
