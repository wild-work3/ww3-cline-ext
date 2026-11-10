#!/usr/bin/env bash
# cline-ext 独立构建脚本：不依赖工作区根脚本，克隆本仓后 `bash build.sh` 即可产出二进制。
# 产物：本仓 dist/cline-ext[.exe]
# 用法：bash build.sh [--target os/arch] [--clean] [--debug] [--zip]
#   --target 交叉编译目标（windows|linux|darwin / amd64|arm64），缺省 = 本机
#   --clean  只清本仓 dist/ 里的产物（不动运行数据目录）
#   --debug  保留符号表/DWARF 与本机路径，便于本地排障
#   --zip    再打一个插件包 dist/<id>-<version>-<os>-<arch>.zip（包根 = exe + manifest.json）
#
# 包名带平台后缀是强制约定（W3-55）：同一版本号要为 6 个平台各发一个包，
# 注册文件的 versions[].file 靠它区分。单一可执行文件在各平台的形态：
#   Windows  <stem>.exe   PE/COFF
#   Linux    <stem>       ELF，无扩展名
#   macOS    <stem>       Mach-O，无扩展名
set -euo pipefail
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"   # --help 里读自己：cd 之后 $0 的相对路径就失效了
cd "$(dirname "$0")"

STEM="cline-ext"              # 可执行文件词干；Windows 追加 .exe，其余平台无扩展名
PKG="."
OUT="$PWD/dist"
BUILD_FLAGS=(-trimpath "-ldflags=-s -w")   # -trimpath 见 README：不把本机路径烧进公开产物
DO_ZIP=0
TARGET=""                  # 空 = 本机

while [ $# -gt 0 ]; do
  case "$1" in
    --target)   [ $# -ge 2 ] || { echo "--target 需要 os/arch" >&2; exit 2; }; TARGET="$2"; shift 2 ;;
    --target=*) TARGET="${1#--target=}"; shift ;;
    --clean)
      for f in "$OUT"/*.exe "$OUT"/*.zip "$OUT"/*.sha256 "$OUT"/manifest.json "$OUT/$STEM"; do
        if [ -e "$f" ]; then
          rm -f "$f" 2>/dev/null || echo "[clean] 进程占用，跳过：$(basename "$f")"
        fi
      done
      rm -rf "$OUT/.data"; shift ;;
    --debug) BUILD_FLAGS=(); shift ;;
    --zip)   DO_ZIP=1; shift ;;
    -h|--help) sed -n '2,17p' "$SELF"; exit 0 ;;
    *) echo "未知参数: $1（可用 --target / --clean / --debug / --zip）" >&2; exit 2 ;;
  esac
done

# 目标平台：显式 --target 优先，否则取 go 的本机默认值
if [ -n "$TARGET" ]; then
  GOOS_T="${TARGET%%/*}"; GOARCH_T="${TARGET##*/}"
else
  GOOS_T=$(go env GOOS); GOARCH_T=$(go env GOARCH)
fi
case "$GOOS_T/$GOARCH_T" in
  */*) ;;
  *) echo "--target 格式应为 os/arch（如 linux/arm64）" >&2; exit 2 ;;
esac

EXE="$STEM"
[ "$GOOS_T" = "windows" ] && EXE="$STEM.exe"

mkdir -p "$OUT"
echo "[build] cline-ext -> dist/$EXE  target=$GOOS_T/$GOARCH_T  flags=${BUILD_FLAGS[*]:-none}"
CGO_ENABLED=0 GOOS="$GOOS_T" GOARCH="$GOARCH_T" go build "${BUILD_FLAGS[@]}" -o "$OUT/$EXE" "$PKG"

# manifest 由刚产出的二进制自己吐：保证包内清单与二进制同源，不手抄、不在仓里另存一份。
# 交叉编译时目标二进制在本机跑不起来，另建一个本机 helper 出清单——manifest 是纯 JSON
# 描述（不含平台信息），谁吐都等价。
if [ "$DO_ZIP" = 1 ]; then
  HOST_OS=$(go env GOOS); HOST_ARCH=$(go env GOARCH)
  MF_BIN="$OUT/$EXE"
  CLEAN_MF=0
  if [ "$GOOS_T" != "$HOST_OS" ] || [ "$GOARCH_T" != "$HOST_ARCH" ]; then
    MF_BIN="$OUT/.mf-helper"
    [ "$HOST_OS" = "windows" ] && MF_BIN="$MF_BIN.exe"
    go build "${BUILD_FLAGS[@]}" -o "$MF_BIN" "$PKG"
    CLEAN_MF=1
  fi
  # 临时数据根：插件 `--data` 缺省是 exe 所在目录，不掺会把 logs/ 落在 dist/ 里
  WW3_DATA="$OUT/.data" "$MF_BIN" manifest --out "$OUT/manifest.json"
  rm -rf "$OUT/.data"
  [ "$CLEAN_MF" = 1 ] && rm -f "$MF_BIN"
  id=$(sed -n 's/.*"id":[[:space:]]*"\([^"]*\)".*/\1/p' "$OUT/manifest.json" | head -1)
  ver=$(sed -n 's/.*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$OUT/manifest.json" | head -1)
  if [ -z "$id" ] || [ -z "$ver" ]; then
    echo "[zip] manifest 缺 id/version" >&2; exit 1
  fi
  pkgname="$id-$ver-$GOOS_T-$GOARCH_T.zip"
  ZIPPER=$(command -v zip || true)
  [ -n "$ZIPPER" ] || ZIPPER=$(command -v bsdtar || true)
  if [ -z "$ZIPPER" ]; then
    for t in /c/Windows/System32/tar.exe "$SYSTEMROOT/System32/tar.exe"; do
      [ -f "$t" ] && ZIPPER="$t" && break
    done
  fi
  if [ -z "$ZIPPER" ]; then
    echo "[zip] 找不到 zip / bsdtar / tar.exe，无法打插件包" >&2; exit 1
  fi
  rm -f "$OUT/$pkgname"
  case "$ZIPPER" in
    */zip) ( cd "$OUT" && "$ZIPPER" -q "$pkgname" "$EXE" manifest.json ) ;;
    *)     ( cd "$OUT" && "$ZIPPER" -a -cf "$pkgname" "$EXE" manifest.json ) ;;
  esac
  if command -v sha256sum >/dev/null; then
    ( cd "$OUT" && sha256sum "$pkgname" | tee "$pkgname.sha256" )
  elif command -v shasum >/dev/null; then
    # macOS 自带 shasum，没有 sha256sum
    ( cd "$OUT" && shasum -a 256 "$pkgname" | tee "$pkgname.sha256" )
  fi
  echo "[zip] dist/$pkgname"
fi

ls -l "$OUT" | awk 'NR>1 && $NF ~ /\.(exe|zip)$/ {printf "  %-40s %8.2f MB\n", $NF, $5/1048576}'
