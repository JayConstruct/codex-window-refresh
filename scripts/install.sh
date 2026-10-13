#!/usr/bin/env bash
set -euo pipefail

restart=true
arguments=()
preview=false
for argument in "$@"; do
  case "$argument" in
    --help|-h)
      printf '%s\n' \
        '用法：bash install.sh [install.py 参数] [--no-restart]' \
        '自动安装依赖、识别 CPA、安装最新版本（含 rc）并重启核实的 systemd 服务。' \
        '首次安装默认仅查询额度；在面板填写模型后可开启模型唤醒。升级保留策略、模型和状态。' \
        '常用参数：--cpa-dir 路径、--model 模型、--service 服务、--user-service --user 用户、--version 版本。' \
        '--no-restart：不自动重启；--dry-run：只预览，不安装依赖；--check-model：只诊断模型。'
      exit 0
      ;;
    --no-restart) restart=false ;;
    --dry-run|--check-model)
      preview=true
      restart=false
      arguments+=("$argument")
      ;;
    --uninstall)
      restart=false
      arguments+=("$argument")
      ;;
    *) arguments+=("$argument") ;;
  esac
done

if [[ $(id -u) != 0 ]]; then
  printf '请以 root 运行：sudo bash install.sh。\n' >&2
  exit 1
fi
os_release=$(cat /etc/os-release)
if [[ ! $os_release =~ (^|$'\n')ID=\"?debian\"?($|$'\n') ||
      ! $os_release =~ (^|$'\n')VERSION_ID=\"?(12|13)\"?($|$'\n') ]]; then
  printf '一键安装仅支持 Debian 12/13；其他系统请参照 docs/install.md。\n' >&2
  exit 1
fi
case "$(uname -m)" in
  x86_64|aarch64) ;;
  *) printf '仅支持 amd64 和 arm64。\n' >&2; exit 1 ;;
esac

dependencies_ready=true
for package in ca-certificates curl python3 python3-yaml; do
  if [[ $(dpkg-query -W -f='${Status}' "$package" 2>/dev/null || true) != 'install ok installed' ]]; then
    dependencies_ready=false
  fi
done
if ! command -v curl >/dev/null || ! command -v python3 >/dev/null || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  dependencies_ready=false
fi
if [[ $dependencies_ready == false ]]; then
  if [[ $preview == true ]]; then
    printf '预览/诊断需要已有依赖；请先安装 ca-certificates curl python3 python3-yaml。\n' >&2
    exit 1
  fi
  printf '安装运行依赖……\n'
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl python3 python3-yaml
fi

script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
installer_file=$script_directory/install.py
if [[ ! -f $installer_file || ! -f $script_directory/../go.mod ]]; then
  temporary=$(mktemp -d /tmp/codex-window-refresh.XXXXXX)
  trap 'rm -rf -- "$temporary"' EXIT
  installer_file=$temporary/install.py
  curl -fSL --retry 3 --connect-timeout 15 --max-time 180 \
    https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.py \
    -o "$installer_file"
fi
defaults=(--version latest --quota-transport host --defer-model --yes)
if [[ $restart == true ]]; then
  printf '安装后会重启 CPA，可能短暂中断在途请求；可用 --no-restart 推迟重启。\n'
  defaults+=(--restart)
fi
python3 "$installer_file" "${defaults[@]}" "${arguments[@]}"
