#!/usr/bin/env bash
set -euo pipefail

activation=hot
explicit_activation=false
arguments=()
preview=false
diagnostic=false
uninstall=false
select_activation() {
  if [[ $explicit_activation == true && $activation != "$1" ]]; then
    printf '%s\n' '--hot-reload、--restart 和 --no-restart 不能同时使用。' >&2
    exit 2
  fi
  activation=$1
  explicit_activation=true
}
for argument in "$@"; do
  case "$argument" in
    --help|-h)
      printf '%s\n' \
        '用法：bash install.sh [install.py 参数] [--hot-reload|--restart|--no-restart]' \
        '仅更新 V1 最新版本（含 rc），默认热重载，不重启 CPA；V2 请使用 scripts/install-v2.sh。' \
        '首次安装默认仅查询额度；在面板填写模型后可开启模型唤醒。升级保留策略、模型和状态。' \
        '常用参数：--cpa-dir 路径、--model 模型、--service 服务、--user-service --user 用户、--version 版本。' \
        '--hot-reload：默认模式，通过本机版本文件确认，无需管理 key。' \
        '--restart：普通安装后重启核实的服务；首次安装或旧版无版本文件时使用。' \
        '--no-restart：普通安装但不重启，不执行热重载确认；--dry-run：只预览，不安装依赖；--check-model：只诊断模型。'
      exit 0
      ;;
    --no-restart) select_activation files ;;
    --hot-reload) select_activation hot ;;
    --restart) select_activation restart ;;
    --dry-run)
      preview=true
      arguments+=("$argument")
      ;;
    --check-model)
      preview=true
      diagnostic=true
      arguments+=("$argument")
      ;;
    --uninstall)
      uninstall=true
      arguments+=("$argument")
      ;;
    *) arguments+=("$argument") ;;
  esac
done
if [[ $diagnostic == true || $uninstall == true ]]; then
  if [[ $explicit_activation == false ]]; then
    activation=files
  elif [[ $activation == hot || $diagnostic == true && $activation == restart ]]; then
    printf '%s\n' '模型诊断不能热重载或重启；卸载不能热重载，需要重启时显式加 --restart。' >&2
    exit 2
  fi
fi

printf '%s\n' '此入口仅安装/更新 V1；V2 请使用 scripts/install-v2.sh，不会自动跨版本切换。'
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
case "$activation" in
  hot)
    printf '默认热重载更新，不重启 CPA，也不需要管理 key。首次安装或旧版缺少版本文件时，请显式使用 --restart；失败不会自动改为重启。\n'
    defaults+=(--hot-reload)
    ;;
  restart)
    printf '已选择普通安装并重启 CPA，可能短暂中断在途请求。\n'
    defaults+=(--restart)
    ;;
esac
python3 "$installer_file" "${defaults[@]}" "${arguments[@]}"
