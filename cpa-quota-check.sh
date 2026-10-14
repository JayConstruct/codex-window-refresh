#!/usr/bin/env bash
# 在服务器的 root 用户会话运行：bash cpa-quota-check.sh
# 只读取状态；不联网、不改配置、不重启、不输出管理密钥。
# 可选参数：服务名、主配置绝对路径。
set -euo pipefail
service_name=${1:-cliproxyapi.service}
config_path=${2:-}

printf '检查 root 用户服务：%s\n' "$service_name"
if [[ $(id -u) != 0 ]]; then
  printf '请在拥有该用户服务的 root 会话运行，避免检查到其他用户的同名服务。\n'
  exit 1
fi
systemctl --user --no-pager --full show "$service_name" \
  -p LoadState -p ActiveState -p MainPID -p WorkingDirectory \
  -p FragmentPath -p DropInPaths -p EnvironmentFiles
process_id=$(systemctl --user --no-pager --full show "$service_name" -p MainPID --value)
if [[ ! $process_id =~ ^[1-9][0-9]*$ ]]; then
  printf '该用户服务没有运行中的 MainPID，请确认服务名和服务状态。\n'
  exit 1
fi

python3 - "$process_id" "$config_path" <<'PY'
import pathlib
import re
import sys
import urllib.parse

try:
    import yaml
except ImportError:
    print('缺少 python3-yaml，无法安全解析配置。可安装后重试；本脚本不会自动安装。')
    sys.exit(1)

try:
    proc = pathlib.Path('/proc') / sys.argv[1]
    cwd = (proc / 'cwd').resolve(strict=True)
    args = (proc / 'cmdline').read_bytes().split(b'\0')
    paths = []
    for i, raw in enumerate(args):
        arg = raw.decode(errors='replace')
        if arg in ('-config', '--config') and i + 1 < len(args):
            paths.append(args[i + 1].decode())
        elif arg.startswith(('-config=', '--config=')):
            paths.append(arg.split('=', 1)[1])
    if sys.argv[2]:
        path = pathlib.Path(sys.argv[2])
    elif len(paths) == 1:
        path = pathlib.Path(paths[0])
    elif paths:
        print('启动参数含多个主配置路径，请用第二个参数明确指定。')
        sys.exit(1)
    else:
        candidates = [cwd / name for name in ('config.yaml', 'config.yml') if (cwd / name).is_file()]
        if len(candidates) != 1:
            print('不能唯一确定主配置，请用第二个参数指定绝对路径。')
            sys.exit(1)
        path = candidates[0]
    if not path.is_absolute():
        path = cwd / path
    print('CPA 进程工作目录：', cwd)
    print('CPA 主配置路径：', path)
    data = yaml.safe_load(path.read_text())
    wrapper = data['plugins']['configs']['codex-window-refresh']
    if not isinstance(wrapper, dict):
        raise ValueError()
    url = wrapper.get('management_url', '')
    env = wrapper.get('management_key_env', '')
    transport = wrapper.get('quota_transport', '')
    if transport == 'host':
        print('额度查询方式：宿主账号凭据；不需要管理密钥环境变量。')
    elif transport == 'management':
        print('额度查询方式：兼容管理桥，需要进程管理密钥。')
    else:
        print('额度查询方式未显式指定：rc.9 起默认宿主查询，旧版使用管理桥；请结合插件版本确认。')
    if not isinstance(url, str) or not isinstance(env, str):
        raise ValueError()
    parsed = urllib.parse.urlsplit(url)
    valid_url = (parsed.scheme in ('http', 'https') and parsed.hostname and
                 not parsed.username and not parsed.password and not parsed.query and not parsed.fragment and
                 parsed.path.rstrip('/') in ('', '/v0/management', '/v0/management/api-call', '/v8/management/requests/api-call'))
    print('management_url：', url if valid_url else '缺失或无效（隐藏原值）')
    valid_env = bool(re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', env))
    print('management_key_env：', env if valid_env else '缺失或不是有效的环境变量名（隐藏原值）')
    if valid_env and transport != 'host':
        prefix = env.encode() + b'='
        entries = (proc / 'environ').read_bytes().split(b'\0')
        present = any(item.startswith(prefix) and len(item) > len(prefix) for item in entries)
        print('CPA 实际进程中的密钥环境变量：', '存在且非空（不显示内容）' if present else '缺失或为空')
        if not present:
            print('仅管理桥模式需要修复 EnvironmentFile；rc.9 宿主模式不依赖此变量。')
    if not valid_url and transport != 'host':
        print('修复方向：在 CPA 主配置的插件节点补充正确的本地管理地址。')
    print('说明：网页登录不会把密钥传给 systemd 服务；前台互斥保护不支持不影响额度接口查询。')
except (OSError, ValueError, KeyError, TypeError, UnicodeError, yaml.YAMLError):
    print('无法读取或解析进程/主配置，请确认服务、配置路径和权限。为避免泄露密钥，不输出原始异常或配置。')
    sys.exit(1)
PY
