#!/usr/bin/env bash
set -euo pipefail
directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf '%s\n' '用法：sudo bash install-v2.sh [--cpa-dir 路径] [--restart] [--version v0.2.X-rc.N]' \
    'v2 只读后台：首次切换或接入层变化须显式 --restart；仅后台变化时不重启 CPA。' \
    '成功后清除 v1 数据、旧版本及临时备份；保留现有 v2 数据库。清理旧服务环境须 --restart。' \
    '支持 --service、--user-service --user、--dry-run、--package、--checksums 与 --cpa-url。'
  exit 0
fi
[[ $(id -u) == 0 ]] || { printf '请以 root 运行。\n' >&2; exit 1; }
preview=false
for argument in "$@"; do [[ $argument != --dry-run ]] || preview=true; done
if ! command -v python3 >/dev/null || ! python3 -c "import yaml,ctypes; ctypes.CDLL('libcurl.so.4'); ctypes.CDLL('libjson-c.so.5')" >/dev/null 2>&1 || \
   ! command -v curl >/dev/null; then
  [[ $preview == false ]] || { printf '预览需要已有 python3-yaml、curl、libcurl4 与 libjson-c5。\n' >&2; exit 1; }
  . /etc/os-release
  [[ $ID == debian && ( $VERSION_ID == 12 || $VERSION_ID == 13 ) ]] || { printf '仅支持 Debian 12/13。\n' >&2; exit 1; }
  apt-get update
  curl_package=libcurl4
  [[ $VERSION_ID != 13 ]] || curl_package=libcurl4t64
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl python3 python3-yaml "$curl_package" libjson-c5
fi
if [[ ! -f $directory/install-v2.py || ! -f $directory/install.py ]]; then
  temporary=$(mktemp -d /tmp/cwr-v2-installer.XXXXXX)
  trap 'rm -rf -- "$temporary"' EXIT
  directory=$temporary
  for file in install.py install-v2.py; do
    curl -fSL --retry 3 --connect-timeout 15 --max-time 180 \
      "https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/$file" -o "$directory/$file"
  done
fi
python3 "$directory/install-v2.py" "$@"
