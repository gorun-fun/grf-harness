#!/bin/sh
# plugins/build.sh 插件独立构建（独立 Go 模块，不参与主程序 build.sh）。
# 用法：
#   ./build.sh            当前系统 → plugins/bin/{grf-browser,grf-computer}
#   ./build.sh install    构建并安装到 ~/.grf/plugins/（grf-harness 自动发现接入）
#   ./build.sh all        交叉编译 6 平台 → plugins/dist/（CI 发布用；linux/windows
#                         产物经 upx 压缩——chromedp 体积大，可减 60% 以上）
#   ./build.sh clean      清理产物
# upx 相关：all 目标对 linux/windows 产物调用 upx（装了才压，UPX=0 可禁用）；
# darwin 与 windows/arm64 一律跳过——upx 会破坏 darwin 内嵌的 adhoc 签名
# （拷贝到 macOS 后运行即 SIGKILL），且至 5.2 尚不支持 win64/arm64 PE。
set -e
cd "$(dirname "$0")"
GO=${GO:-go}
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
    GO=/usr/local/go/bin/go
fi

# 版本号：CI 取 tag，本地取 git describe（去 v 前缀），非 git 环境兜底 dev
VERSION=${CI_COMMIT_TAG:-$(cd .. && git describe --tags --always --dirty 2>/dev/null || echo dev)}
case "$VERSION" in v[0-9]*) VERSION=${VERSION#v};; esac
LDFLAGS="-s -w -X main.version=$VERSION"

PLUGINS="browser computer"
bin_name() { # goos → 产物后缀
    [ "$1" = windows ] && echo ".exe" || echo ""
}

# upx_compress 参数：目标 goos、产物路径。压缩失败只警告不中断（保留未压缩版本）。
upx_compress() {
    if [ "$1" = darwin ]; then
        return 0
    fi
    if [ "$1$2" = "windowsarm64" ]; then
        # goos goarch 拼串判断：upx 至 5.2 尚不支持 win64/arm64 PE
        return 0
    fi
    if [ "${UPX:-1}" = "0" ]; then
        return 0
    fi
    if ! command -v upx >/dev/null 2>&1; then
        echo "提示: 未安装 upx，$3 未压缩"
        return 0
    fi
    if ! upx "$3"; then
        echo "警告: upx 压缩 $3 失败，保留未压缩版本"
    fi
}

build_native() {
    echo "== vet (plugins) =="
    "$GO" vet ./...
    mkdir -p bin
    for p in $PLUGINS; do
        echo "== build $p =="
        "$GO" build -trimpath -ldflags "$LDFLAGS" -o "bin/grf-$p" "./$p"
        # macOS：复制后 adhoc 签名可能失效（SIGKILL），重签一次
        if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
            codesign -f -s - "bin/grf-$p" >/dev/null 2>&1 || true
        fi
        echo "OK: bin/grf-$p"
    done
}

case "${1:-}" in
    "" )
        build_native
        ;;
    install )
        build_native
        DEST="${HOME}/.grf/plugins"
        mkdir -p "$DEST"
        BINDEST="${HOME}/bin"
        if [ -n "$HOME" ]; then
            mkdir -p "$BINDEST"
        fi
        for p in $PLUGINS; do
            # macOS：cp 原地覆盖已签名的旧文件会使签名失效（运行时 SIGKILL），
            # 必须先删旧文件（新 inode）并在落点重签
            rm -f "$DEST/grf-$p"
            cp "bin/grf-$p" "$DEST/grf-$p"
            if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
                codesign -f -s - "$DEST/grf-$p" >/dev/null 2>&1 || true
            fi
            echo "已安装: $DEST/grf-$p"
            if [ -n "$HOME" ]; then
                rm -f "$BINDEST/grf-$p"
                cp "bin/grf-$p" "$BINDEST/grf-$p"
                if [ "$(uname)" = "Darwin" ] && command -v codesign >/dev/null 2>&1; then
                    codesign -f -s - "$BINDEST/grf-$p" >/dev/null 2>&1 || true
                fi
                echo "已同步: $BINDEST/grf-${p}（供远程模式手工启动/调试）"
            fi
        done
        echo "grf-harness 启动时会自动发现 $DEST 下的插件（settings.json 配 plugins: [] 可关闭）。"
        ;;
    all )
        echo "== vet (plugins) =="
        "$GO" vet ./...
        mkdir -p dist
        for target in "darwin amd64" "darwin arm64" "linux amd64" "linux arm64" "windows amd64" "windows arm64"; do
            set -- $target
            os=$1; arch=$2; ext=$(bin_name "$os")
            for p in $PLUGINS; do
                echo "== build grf-$p ($os/$arch) =="
                GOOS=$os GOARCH=$arch CGO_ENABLED=0 "$GO" build -trimpath -ldflags "$LDFLAGS" \
                    -o "dist/grf-$p-$os-$arch$ext" "./$p"
                if [ "$os" = darwin ] && command -v codesign >/dev/null 2>&1; then
                    codesign -f -s - "dist/grf-$p-$os-$arch$ext" >/dev/null 2>&1 || true
                fi
                upx_compress "$os" "$arch" "dist/grf-$p-$os-$arch$ext"
                echo "OK: dist/grf-$p-$os-$arch$ext"
            done
        done
        ;;
    clean )
        rm -rf bin dist
        echo "已清理"
        ;;
    * )
        echo "用法: $0 [install|all|clean]（缺省构建当前系统）" >&2
        exit 1
        ;;
esac
