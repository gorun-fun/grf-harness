#!/bin/sh
# build.sh 一键：vet + 编译（跳过测试）
# 用法：./build.sh [目标]
#   （缺省）       当前系统，产物 ./gh
#   macos         darwin/amd64   → dist/gh-darwin-amd64
#   macos-arm64   darwin/arm64   → dist/gh-darwin-arm64
#   linux         linux/amd64    → dist/gh-linux-amd64
#   linux-arm64   linux/arm64    → dist/gh-linux-arm64
#   windows       windows/amd64  → dist/gh-windows-amd64.exe
#   windows-arm64 windows/arm64  → dist/gh-windows-arm64.exe
#   all           上述六个平台全部
# 兼容别名：linux-arm = linux-arm64
set -e
cd "$(dirname "$0")"
GO=${GO:-go}
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
    GO=/usr/local/go/bin/go
fi

# 版本号注入 config.Version：CI tag 构建取 $CI_COMMIT_TAG，本地取 git describe
# （当前 commit 无 tag 时退到短 hash），非 git 环境兜底 dev；统一去掉 v 前缀
VERSION=${CI_COMMIT_TAG:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
case "$VERSION" in v[0-9]*) VERSION=${VERSION#v};; esac
LDFLAGS="-s -w -X git.gorun.fun/share/grf-harness/config.Version=$VERSION"

# 交叉编译单个目标：参数为 名称 GOOS GOARCH 产物路径
# vet 也带 GOOS/GOARCH，可顺带检查平台专属文件（termios/term 回退等）
build_one() {
    echo "== vet ($1: $2/$3) =="
    GOOS=$2 GOARCH=$3 "$GO" vet ./...
    echo "== build ($1: $2/$3) =="
    GOOS=$2 GOARCH=$3 "$GO" build -trimpath -ldflags "$LDFLAGS" -o "$4" .
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
        "$GO" build -trimpath -ldflags "$LDFLAGS" -o gh .
        # macOS：链接器的 adhoc 签名在文件被复制/Finder 拷贝后可能被系统
        # 直接 SIGKILL（zsh: killed）；重新 adhoc 签名一次即可规避
        if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
            codesign -f -s - gh >/dev/null 2>&1 || true
        fi
        echo "OK: ./gh version"
        ./gh version
        # 产物同步到 ~/bin（创建目录），命令行任意目录可直接用 gh
        if [ -n "$HOME" ]; then
            mkdir -p "$HOME/bin"
            # 原地覆盖已签名旧文件会触发 macOS 签名校验 SIGKILL，先删再拷
            rm -f "$HOME/bin/gh"
            cp gh "$HOME/bin/gh"
            if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
                codesign -f -s - "$HOME/bin/gh" >/dev/null 2>&1 || true
            fi
            echo "已同步: $HOME/bin/gh"
        fi
        # 插件（独立 Go 模块）一并构建并安装到 ~/.grf/plugins/；
        # 构建失败不吞掉（set -e），CI 交叉编译场景不在此路径
        ./plugins/build.sh install
        ;;
    macos)
        mkdir -p dist
        build_one macos darwin amd64 dist/gh-darwin-amd64
        ;;
    macos-arm64)
        mkdir -p dist
        build_one macos-arm64 darwin arm64 dist/gh-darwin-arm64
        ;;
    linux)
        mkdir -p dist
        build_one linux linux amd64 dist/gh-linux-amd64
        ;;
    linux-arm64|linux-arm)
        mkdir -p dist
        build_one linux-arm64 linux arm64 dist/gh-linux-arm64
        ;;
    windows)
        mkdir -p dist
        build_one windows windows amd64 dist/gh-windows-amd64.exe
        ;;
    windows-arm64)
        mkdir -p dist
        build_one windows-arm64 windows arm64 dist/gh-windows-arm64.exe
        ;;
    all)
        mkdir -p dist
        build_one macos darwin amd64 dist/gh-darwin-amd64
        build_one macos-arm64 darwin arm64 dist/gh-darwin-arm64
        build_one linux linux amd64 dist/gh-linux-amd64
        build_one linux-arm64 linux arm64 dist/gh-linux-arm64
        build_one windows windows amd64 dist/gh-windows-amd64.exe
        build_one windows-arm64 windows arm64 dist/gh-windows-arm64.exe
        ;;
    *)
        echo "用法: $0 [macos|macos-arm64|linux|linux-arm64|windows|windows-arm64|all]（缺省编译当前系统）" >&2
        exit 1
        ;;
esac
