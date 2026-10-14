#!/usr/bin/env bash
# 在服务器 root 会话运行：bash cpa-quota-fix.sh
# 仅用于旧版或明确选择 management 管理桥；rc.9 默认宿主查询无需运行。
# 针对 /root/cliproxyapi 的 root 用户服务 cliproxyapi.service。
# 会备份并配置 EnvironmentFile，然后重启该用户服务。
# 优先复用已有密钥文件；需要填写时仅在服务器本地隐藏输入。
set -euo pipefail
service_name=cliproxyapi.service
cpa_directory=/root/cliproxyapi
environment_path=$cpa_directory/window-refresh.env
override_directory=/root/.config/systemd/user/$service_name.d
override_path=$override_directory/99-window-refresh-quota-env.conf

if [[ $(id -u) != 0 ]]; then
  printf '请在 root 用户会话执行。\n'
  exit 1
fi
if [[ $(systemctl --user --no-pager show "$service_name" -p LoadState --value) != loaded ]]; then
  printf '没有找到 root 的 cliproxyapi.service，未修改任何文件。\n'
  exit 1
fi
if [[ $(systemctl --user --no-pager show "$service_name" -p WorkingDirectory --value) != "$cpa_directory" ]]; then
  printf '服务工作目录与 /root/cliproxyapi 不一致，已停止。\n'
  exit 1
fi

python3 - "$environment_path" "$override_path" <<'PY'
import getpass
import os
import pathlib
import re
import shutil
import sys
import tempfile
import time

env_path, override_path = map(pathlib.Path, sys.argv[1:])
stamp = time.strftime('%Y%m%d-%H%M%S') + '-' + str(os.getpid())

def check_file(path):
    if path.is_symlink() or (path.exists() and (not path.is_file() or path.stat().st_uid != 0)):
        print('文件不是 root 拥有的普通文件，已停止：', path)
        sys.exit(1)

def backup(path):
    if path.exists():
        saved = path.with_name(path.name + '.bak-' + stamp)
        shutil.copy2(path, saved)
        os.chmod(saved, 0o600)
        print('已备份：', saved)

def write(path, content, mode):
    path.parent.mkdir(parents=True, exist_ok=True)
    check_file(path)
    fd, temporary = tempfile.mkstemp(prefix='.window-refresh-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as file:
            os.fchmod(file.fileno(), mode)
            file.write(content)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)

try:
    # Validate both destinations before making changes.
    check_file(env_path)
    check_file(override_path)
    text = env_path.read_text() if env_path.exists() else ''
    assignments = re.findall(r'^\s*CPA_MANAGEMENT_KEY\s*=([^\n]*)', text, flags=re.MULTILINE)
    populated = bool(assignments and assignments[-1].strip() not in ('', '""', "''"))
    if populated:
        print('复用已有环境文件中的 CPA_MANAGEMENT_KEY，不显示密钥。')
        os.chmod(env_path, 0o600)
    else:
        key = getpass.getpass('请输入网页登录 CPA 使用的原始管理密钥（隐藏输入，不是 API Key 或 bcrypt 哈希）：')
        if not key or any(ord(char) < 32 or ord(char) == 127 for char in key):
            print('管理密钥为空或包含控制字符，未写入配置。')
            sys.exit(1)
        encoded = key.replace('\\', '\\\\').replace('"', '\\"')
        assignment = 'CPA_MANAGEMENT_KEY="' + encoded + '"\n'
        # Preserve unrelated environment assignments; replace this key only.
        text = re.sub(r'^\s*CPA_MANAGEMENT_KEY\s*=[^\n]*(?:\n|$)', '', text, flags=re.MULTILINE)
        backup(env_path)
        write(env_path, text + ('\n' if text and not text.endswith('\n') else '') + assignment, 0o600)
        key = encoded = assignment = ''
        print('管理密钥已保存到 root 私有环境文件，不显示内容。')
    backup(override_path)
    write(override_path, '# CPA quota query environment\n[Service]\nEnvironmentFile=' + str(env_path) + '\n', 0o644)
    print('已配置 root 用户服务的环境文件：', override_path)
except (OSError, UnicodeError, EOFError):
    print('无法读取、输入或写入配置；不输出原始异常或文件内容。服务尚未重启。')
    sys.exit(1)
PY

systemctl --user daemon-reload
systemctl --user restart "$service_name"
systemctl --user is-active "$service_name"
process_id=$(systemctl --user --no-pager show "$service_name" -p MainPID --value)
python3 - "$process_id" <<'PY'
import pathlib
import sys

try:
    entries = (pathlib.Path('/proc') / str(int(sys.argv[1])) / 'environ').read_bytes().split(b'\0')
    prefix = b'CPA_MANAGEMENT_KEY='
    present = any(entry.startswith(prefix) and len(entry) > len(prefix) for entry in entries)
except (OSError, ValueError):
    print('无法检查新进程环境，请运行排查脚本确认。')
    sys.exit(1)
if not present:
    print('新进程仍未收到管理密钥。请运行排查脚本，检查其他 drop-in 或启动脚本是否清除了变量。')
    sys.exit(1)
print('验证通过：新的 CPA 进程已收到非空 CPA_MANAGEMENT_KEY，未显示密钥内容。')
print('现在刷新 CPA 网页，等待接口能力更新，再点击“查询此账号额度”。')
print('该检查不调用上游额度或模型接口；密钥是否正确需实际额度查询确认。')
PY
