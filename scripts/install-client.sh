#!/usr/bin/env bash
set -euo pipefail

CLIENT_VERSION=v0.2.0-rc.27
CLIENT_SHA256=0608a3f1dd4c827fd6fb2e1ffb613877db0ab19716a72c9b94938fe5815ba37b
version=$CLIENT_VERSION
bin_dir=${HOME:-}/.local/bin
while [[ $# -gt 0 ]]; do
  case "$1" in
    --bin-dir)
      [[ $# -ge 2 && -n $2 ]] || { printf '请为 --bin-dir 指定安装目录。\n' >&2; exit 1; }
      bin_dir=$2; shift 2 ;;
    --help|-h)
      printf '%s\n' '用法：bash install-client.sh [--bin-dir /path/to/bin]' \
        "安装或更新本安装器配套的客户端 $CLIENT_VERSION，默认目录为 ~/.local/bin。" \
        '需要 Python 3.10+ 和 curl，支持 Linux/macOS，无需 sudo。' \
        '提供 cwr-codex 命令及 cwr-codex.py；不修改 Codex 配置、目录映射或 shell 配置。' \
        '直接运行默认使用 cliproxyapi；提供商报错时可输入 ID 并记住，下次无需再填。' \
        '再次下载并运行安装器即可更新；可在已解压的发行包目录中离线安装。'
      exit 0 ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 1 ;;
  esac
done
command -v python3 >/dev/null || { printf '请先安装 Python 3.10+。\n' >&2; exit 1; }
python3 -c 'import sys; sys.exit(sys.version_info < (3, 10))' || { printf '客户端需要 Python 3.10+。\n' >&2; exit 1; }
[[ -n ${HOME:-} || $bin_dir != /.local/bin ]] || { printf '请设置 HOME 或使用 --bin-dir 指定目录。\n' >&2; exit 1; }

temporary=$(mktemp -d "${TMPDIR:-/tmp}/cwr-client.XXXXXX")
trap 'rm -rf -- "$temporary"' EXIT
directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ -f $directory/cwr-codex.py ]]; then
  cp -- "$directory/cwr-codex.py" "$temporary/cwr-codex.py"
else
  command -v curl >/dev/null || { printf '请先安装 curl。\n' >&2; exit 1; }
  curl -fSL --retry 3 --connect-timeout 15 --max-time 180 -H 'Cache-Control: no-cache' \
    "https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/$version/scripts/cwr-codex.py" \
    -o "$temporary/cwr-codex.py"
fi

python3 - "$temporary/cwr-codex.py" "$CLIENT_SHA256" "$bin_dir" <<'PY'
import ast
import hashlib
import os
from pathlib import Path
import sys
import tempfile

source, expected, target = sys.argv[1:]
try:
    content = Path(source).read_bytes()
    if hashlib.sha256(content).hexdigest() != expected:
        raise ValueError("客户端脚本 SHA-256 校验失败，原安装未修改")
    ast.parse(content, filename="cwr-codex.py")
    destination = Path(target).expanduser().resolve()
    destination.mkdir(parents=True, exist_ok=True)
    script, command = destination / "cwr-codex.py", destination / "cwr-codex"
    if script.is_dir() or command.is_dir():
        raise ValueError("安装目标是目录，请使用 --bin-dir 指定其他位置")
    staged_script = staged_link = None
    try:
        with tempfile.NamedTemporaryFile(dir=destination, prefix=".cwr-client-", delete=False) as stream:
            staged_script = Path(stream.name)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        staged_script.chmod(0o755)
        # Stage the link before replacing the installed script.
        descriptor, name = tempfile.mkstemp(dir=destination, prefix=".cwr-command-")
        os.close(descriptor)
        staged_link = Path(name)
        staged_link.unlink()
        staged_link.symlink_to("cwr-codex.py")
        os.replace(staged_script, script)
        os.replace(staged_link, command)
    finally:
        for path in (staged_script, staged_link):
            if path is not None:
                path.unlink(missing_ok=True)
    print("客户端已安装：" + str(command))
    if str(destination) not in os.environ.get("PATH", "").split(os.pathsep):
        print("请将此目录加入 PATH：" + str(destination))
except (OSError, ValueError, SyntaxError) as error:
    sys.exit("客户端安装失败：" + str(error))
PY
printf '版本：%s。进入项目目录运行 cwr-codex，或使用安装输出中的完整路径。\n' "$version"
