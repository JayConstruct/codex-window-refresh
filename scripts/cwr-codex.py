#!/usr/bin/env python3
"""Launch Codex in CPA scheduling mode or with official ChatGPT sign-in."""
import argparse
import hashlib
import json
import ntpath
import os
import posixpath
from pathlib import Path
import re
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import quote, urlsplit, urlunsplit

MODES = ("auto", "cpa", "official")
OFFICIAL_CONFIG = {
    "model_provider": "openai",
    "forced_login_method": "chatgpt",
    "openai_base_url": "https://chatgpt.com/backend-api/codex",
    "chatgpt_base_url": "https://chatgpt.com/backend-api/",
}
CODEX_VALUE_OPTIONS = {"-c", "--config", "-p", "--profile", "-m", "--model", "-s", "--sandbox",
                       "-a", "--ask-for-approval", "-i", "--image", "--remote", "--remote-auth-token-env",
                       "--local-provider", "--add-dir", "--enable", "--disable", "-o", "--output-last-message",
                       "--output-schema"}

CODEMAN_SETUP = r"""
import {pathToFileURL} from 'node:url';
import {join} from 'node:path';
let input = '';
for await (const chunk of process.stdin) input += chunk;
const {config, launcher} = JSON.parse(input);
const directory = process.argv[1];
const {STOCK_CLIS} = await import(pathToFileURL(join(directory, 'stock.js')));
const {resolveRegistry} = await import(pathToFileURL(join(directory, 'registry.js')));
const warnings = [];
const entry = resolveRegistry(STOCK_CLIS, config, warnings).entries.find(entry => entry.id === 'codex');
if (!entry || warnings.length) throw new Error('invalid_registry');
const variants = entry.launch.variants.map(variant => {
  const args = variant.args;
  if (args[0]?.valueFrom === 'cwrExecutable' && args[1]?.lit === '--') return variant;
  if (args[0]?.lit !== 'codex') throw new Error('custom_codex_command');
  return {...variant, args: [{valueFrom: 'cwrExecutable', quote: 'single'}, {lit: '--'}, ...args.slice(1)]};
});
config.clis.codex ??= {};
config.clis.codex.launch ??= {};
config.clis.codex.launch.params ??= {};
config.clis.codex.launch.params.cwrExecutable = {type: 'enum', values: [launcher], default: launcher};
config.clis.codex.launch.variants = variants;
const validated = [];
resolveRegistry(STOCK_CLIS, config, validated);
if (validated.length) throw new Error('unsupported_registry');
process.stdout.write(JSON.stringify(config, null, 2) + '\n');
"""


def configure_codeman(directory):
    """Use Codeman's own registry to preserve its current Codex launch options."""
    root = Path(directory).expanduser().resolve(strict=True)
    modules = root / "app" / "dist" / "config" / "cli-registry"
    if not all((modules / name).is_file() for name in ("stock.js", "registry.js")):
        raise ValueError("未找到 Codeman CLI 注册表模块，请指定正确的 --setup-codeman 目录或更新 Codeman")
    node = shutil.which("node")
    if not node:
        raise ValueError("配置 Codeman 入口需要 Node.js，请在能运行 Codeman 的环境中执行安装器")
    path = root / "clis.json"
    original = None
    config = {"schemaVersion": 1, "clis": {}}
    if path.exists():
        if path.stat().st_size > 1024 * 1024:
            raise ValueError("Codeman CLI 注册表过大，原文件未修改")
        original = path.read_bytes()
        try:
            config = json.loads(original)
        except (ValueError, UnicodeError):
            raise ValueError("Codeman CLI 注册表不是有效 JSON，原文件未修改") from None
        if not isinstance(config, dict) or not isinstance(config.get("clis"), dict):
            raise ValueError("Codeman CLI 注册表格式无效，原文件未修改")
    payload = json.dumps({"config": config, "launcher": str(Path(__file__).absolute())})
    result = subprocess.run([node, "--input-type=module", "-e", CODEMAN_SETUP, str(modules)],
                            input=payload, capture_output=True, text=True, timeout=15)
    if result.returncode:
        raise ValueError("Codeman CLI 注册表校验失败或已有自定义 Codex 启动命令，原文件未修改")
    content = result.stdout.encode("utf-8")
    json.loads(content)  # Refuse an incomplete helper result before touching the original.
    if original == content and (path.stat().st_mode & 0o777) == 0o600:
        return path
    backup = root / "clis.json.cwr-backup"
    if original is not None and not backup.exists():
        with os.fdopen(os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "wb") as stream:
            stream.write(original)
    staged = None
    try:
        with tempfile.NamedTemporaryFile(dir=root, prefix=".cwr-codeman-", delete=False) as stream:
            staged = Path(stream.name)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(staged, path)
    finally:
        if staged is not None:
            staged.unlink(missing_ok=True)
    return path


def valid_provider(value):
    return isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,64}", value) is not None


def client_settings_path():
    return Path(os.environ.get("XDG_CONFIG_HOME") or "~/.config").expanduser() / "cwr" / "client.json"


def client_settings():
    """Read only our preferences; malformed settings must not silently switch routes."""
    try:
        path = client_settings_path()
        if not path.exists():
            return {}
        if path.stat().st_size > 4096:
            raise ValueError("客户端设置文件过大")
        data = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(data, dict) or ("provider" in data and not valid_provider(data["provider"])) \
                or ("mode" in data and data["mode"] not in MODES) \
                or ("cpa_urls" in data and (not isinstance(data["cpa_urls"], list)
                    or len(data["cpa_urls"]) > 32
                    or any(cpa_url(url) != url for url in data["cpa_urls"]))):
            raise ValueError("客户端设置无效")
        return data
    except (OSError, ValueError):
        raise ValueError("无法读取客户端设置，请检查 " + str(client_settings_path()) + "；未启动 Codex") from None


def save_settings(**updates):
    data = {**client_settings(), **updates}
    content = json.dumps(data, ensure_ascii=False) + "\n"
    if len(content.encode("utf-8")) > 4096:
        raise ValueError("客户端设置超过 4096 字节，请减少 CPA 地址或其他设置")
    path = client_settings_path()
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    staged = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=".client-", delete=False) as stream:
            staged = Path(stream.name)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(staged, path)
    finally:
        if staged is not None:
            staged.unlink(missing_ok=True)


def run_codex(argv, env=None):
    """Preserve stdin/stdout terminals; relay and retain only bounded stderr."""
    tail = bytearray()
    process = subprocess.Popen(argv, stderr=subprocess.PIPE, bufsize=0, env=env)

    def relay():
        try:
            while chunk := process.stderr.read(4096):
                tail.extend(chunk)
                del tail[:-65536]
                try:
                    output = getattr(sys.stderr, "buffer", None)
                    if output is None:
                        sys.stderr.write(chunk.decode("utf-8", errors="replace"))
                    else:
                        output.write(chunk)
                    sys.stderr.flush()
                except OSError:
                    pass
        except OSError:
            pass

    reader = threading.Thread(target=relay, daemon=True)
    reader.start()
    previous = {}
    try:
        for signum in (signal.SIGTERM, getattr(signal, "SIGHUP", signal.SIGTERM)):
            if signum in previous:
                continue
            previous[signum] = signal.getsignal(signum)
            signal.signal(signum, lambda received, _frame: process.send_signal(received) if process.poll() is None else None)
        while True:
            try:
                code = process.wait()
                break
            except KeyboardInterrupt:
                if process.poll() is None:
                    process.send_signal(signal.SIGINT)
        reader.join(timeout=1)
        return code if code >= 0 else 128 - code, bytes(tail).decode("utf-8", errors="replace")
    finally:
        for signum, handler in previous.items():
            signal.signal(signum, handler)
        process.stderr.close()


def directory_project(directory):
    """Use the nearest Git root, or the launch directory outside a checkout."""
    root = Path(directory).resolve()
    for candidate in (root, *root.parents):
        git = candidate / ".git"
        if git.is_file() or (git / "HEAD").is_file():
            root = candidate
            break
    canonical = os.path.normcase(str(root)).replace(os.sep, "/")
    digest = hashlib.sha256(("cwr-directory-v1\0" + os.name + "\0" + canonical).encode("utf-8")).hexdigest()
    return root, "dir-" + digest


def project_for(directory, mappings, fallback=None):
    directory = Path(directory).resolve()
    matches = []
    roots = set()
    for item in mappings:
        # A downloaded map can describe several clients. Only resolve paths for
        # this OS; do not reinterpret another platform's absolute path as local.
        raw = os.path.expanduser(item["directory"])
        windows_absolute = bool(ntpath.splitdrive(raw)[0]) and ntpath.isabs(raw)
        if os.name != "nt" and windows_absolute and not raw.startswith("/"):
            continue
        if os.name == "nt" and posixpath.isabs(raw) and not windows_absolute:
            continue
        root = Path(raw)
        if not root.is_absolute():
            raise ValueError("目录映射必须使用绝对路径")
        root = root.resolve()
        if root in roots:
            raise ValueError("目录映射重复")
        roots.add(root)
        if directory == root or root in directory.parents:
            matches.append((len(root.parts), item["project"]))
    if not matches:
        if fallback is not None:
            return fallback
        raise ValueError("当前目录没有项目映射，请指定 --project 或配置 --map")
    return max(matches)[1]


def launch_directory(arguments, fallback):
    """Honor native Codex directory flags without interpreting option values or -- prompts."""
    directory, extra, index = None, [], 0
    while index < len(arguments):
        value = arguments[index]
        if value == "--":
            extra.extend(arguments[index:])
            break
        if value in ("-C", "--cd") or value.startswith(("--cd=", "-C")):
            if directory is not None:
                raise ValueError("请只指定一次 -C 或 --cd 启动目录")
            if value in ("-C", "--cd"):
                index += 1
                if index >= len(arguments):
                    raise ValueError("请为 -C 或 --cd 指定启动目录")
                directory = arguments[index]
            else:
                directory = value.split("=", 1)[1] if value.startswith("--cd=") else value[2:].removeprefix("=")
            if not directory:
                raise ValueError("启动目录不能为空")
        else:
            extra.append(value)
            if value in CODEX_VALUE_OPTIONS and index + 1 < len(arguments):
                index += 1
                extra.append(arguments[index])
        index += 1
    return Path(directory if directory is not None else fallback).expanduser().resolve(strict=True), extra


def official_arguments(arguments):
    """Reject route overrides while leaving model, profile, resume and prompt arguments intact."""
    index = 0
    while index < len(arguments):
        value = arguments[index]
        if value == "--":
            break
        if value.split("=", 1)[0] in ("--oss", "--local-provider", "--remote"):
            raise ValueError("官方模式不能使用本地模型或远程服务参数；可用 --mode auto 保留原生启动方式")
        config = None
        if value in ("-c", "--config") and index + 1 < len(arguments):
            index += 1
            config = arguments[index]
        elif value.startswith("--config="):
            config = value[len("--config="):]
        elif value.startswith("-c") and value != "-c":
            config = value[2:].removeprefix("=")
        elif value in CODEX_VALUE_OPTIONS and index + 1 < len(arguments):
            index += 1
        if config is not None:
            key = config.split("=", 1)[0].strip().split(".", 1)[0].strip().strip("\"'")
            if key in OFFICIAL_CONFIG or key == "model_providers":
                raise ValueError("官方模式已固定提供商、登录方式和官方地址，请移除冲突的 -c/--config 参数")
        index += 1
    return [item for key, value in OFFICIAL_CONFIG.items() for item in ("-c", key + "=" + json.dumps(value))]


def official_environment():
    # Scope routing/key overrides to the child. Preserve proxies, custom CAs and all other variables.
    excluded = {"OPENAI_BASE_URL", "CHATGPT_BASE_URL", "OPENAI_API_KEY", "CODEX_API_KEY"}
    return {key: value for key, value in os.environ.items() if key not in excluded}


def cpa_url(value):
    """Normalize an exact CPA base URL, rejecting official destinations and hidden URL components."""
    if not isinstance(value, str) or not value or any(ord(ch) <= 32 or ord(ch) >= 127 for ch in value) \
            or "\\" in value or "%" in value:
        raise ValueError("CPA 地址必须是完整的 HTTP/HTTPS 地址，不能包含凭据、查询参数或片段")
    parsed = urlsplit(value)
    host = (parsed.hostname or "").lower().rstrip(".")
    if parsed.scheme not in ("http", "https") or not host or parsed.username is not None \
            or parsed.password is not None or parsed.query or parsed.fragment or "?" in value or "#" in value:
        raise ValueError("CPA 地址必须是完整的 HTTP/HTTPS 地址，不能包含凭据、查询参数或片段")
    if any(host == domain or host.endswith("." + domain) for domain in ("openai.com", "chatgpt.com", "chat.com")):
        raise ValueError("官方地址不能登记为 CPA，也不会添加项目头")
    try:
        port = parsed.port
    except ValueError:
        raise ValueError("CPA 地址端口无效") from None
    if ":" in host:
        host = "[" + host + "]"
    authority = host if port is None or port == (443 if parsed.scheme == "https" else 80) else host + ":" + str(port)
    return urlunsplit((parsed.scheme, authority, parsed.path.rstrip("/"), "", ""))


def probe_options(arguments):
    """Forward configuration overrides, never prompts or execution commands, to the config reader."""
    options, index = [], 0
    while index < len(arguments):
        value = arguments[index]
        if value == "--":
            break
        if value.split("=", 1)[0] in ("--oss", "--local-provider", "--remote", "--profile", "-p") \
                or value.startswith("-p") and value != "-p":
            raise ValueError("当前配置检测不支持 profile、本地模型或远程服务参数")
        if value in ("-c", "--config", "--enable", "--disable"):
            if index + 1 >= len(arguments):
                raise ValueError("配置参数缺少值")
            options.extend((value, arguments[index + 1]))
            index += 1
        elif value.startswith(("--config=", "--enable=", "--disable=", "-c")):
            options.append(value)
        elif value in CODEX_VALUE_OPTIONS and index + 1 < len(arguments):
            index += 1
        index += 1
    return options


def read_route(codex, directory, arguments):
    """Read layered Codex configuration with bounded stdio RPC; never start a thread or turn."""
    options = probe_options(arguments)
    process = subprocess.Popen([codex, "-C", str(directory), *options, "app-server", "--listen", "stdio://"],
                               cwd=directory, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.DEVNULL, bufsize=0, start_new_session=True)
    pending = bytearray()
    deadline = time.monotonic() + 4
    try:
        def send(message):
            process.stdin.write((json.dumps(message) + "\n").encode())

        def response(identity):
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                while True:
                    while b"\n" in pending:
                        line, _, tail = pending.partition(b"\n")
                        pending[:] = tail
                        message = json.loads(line)
                        if not isinstance(message, dict):
                            raise ValueError("Codex 配置响应不可用")
                        if message.get("id") == identity:
                            if not isinstance(message.get("result"), dict):
                                raise ValueError("Codex 配置读取失败")
                            return message["result"]
                    remaining = deadline - time.monotonic()
                    if remaining <= 0 or not selector.select(remaining):
                        raise ValueError("Codex 配置读取超时")
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk or len(pending) + len(chunk) > 2 * 1024 * 1024:
                        raise ValueError("Codex 配置响应不可用")
                    pending.extend(chunk)

        send({"id": 1, "method": "initialize", "params": {"clientInfo": {"name": "cwr_config_reader", "version": "1"}}})
        response(1)
        send({"method": "initialized"})
        send({"id": 2, "method": "config/read", "params": {"includeLayers": False, "cwd": str(directory)}})
        config = response(2).get("config")
        send({"id": 3, "method": "configRequirements/read", "params": {}})
        requirements_result = response(3)
        if "requirements" not in requirements_result:
            raise ValueError("Codex 未返回可核实的提供商策略")
        requirements = requirements_result["requirements"]
        if not isinstance(config, dict) or (requirements is not None and not isinstance(requirements, dict)):
            raise ValueError("Codex 配置响应不可用")
        if requirements and any(requirements.get(key) for key in ("model_provider", "model_providers", "modelProvider", "modelProviders")):
            raise ValueError("组织策略管理了提供商，无法固定 CPA 地址")
        if config.get("profile"):
            raise ValueError("当前配置检测不支持 profile")
        provider = config.get("model_provider") or "openai"
        providers = config.get("model_providers") or {}
        if not valid_provider(provider) or not isinstance(providers, dict):
            raise ValueError("Codex 提供商不可用")
        if provider in ("openai", "ollama", "lmstudio", "amazon-bedrock", "amazon-bedrock-runtime", "gcp"):
            return provider, None
        definition = providers.get(provider)
        if not isinstance(definition, dict):
            raise ValueError("Codex 提供商定义不可用")
        if definition.get("model_catalog_url"):
            raise ValueError("提供商单独指定了模型目录地址，无法确认请求头仅发送给 CPA")
        return provider, cpa_url(definition.get("base_url"))
    finally:
        if process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        process.wait()
        for stream in (process.stdin, process.stdout):
            stream.close()


def scheduling_route(args, directory, arguments):
    urls = getattr(args, "cpa_urls", [])
    if not urls:
        if args.mode == "cpa":
            raise ValueError("请先用 --trust-cpa-url 登记 CPA 完整地址；未添加项目头")
        return None
    try:
        provider, url = read_route(args.codex, directory, arguments)
        if url not in urls or args.provider is not None and provider != args.provider:
            raise ValueError("生效提供商或完整地址未匹配已登记 CPA")
        return provider, url
    except (OSError, ValueError, TypeError):
        if args.mode == "cpa":
            raise ValueError("无法确认生效配置指向已登记 CPA，请检查地址、profile 和提供商；未启动 Codex") from None
        print("cwr-codex：无法确认目标为已登记 CPA，本次不添加项目头。", file=sys.stderr)
        return None


def command(args):
    extra = args.arguments[1:] if args.arguments[:1] == ["--"] else args.arguments
    directory, extra = launch_directory(extra, args.directory)
    if not directory.is_dir():
        raise ValueError("启动目录必须是文件夹")
    argv = [args.codex, "-C", str(directory)]
    if args.mode == "official":
        return [*argv, *official_arguments(extra), *extra]
    if args.provider is not None and not valid_provider(args.provider):
        raise ValueError("调度提供商 ID 无效")
    route = scheduling_route(args, directory, extra)
    if route is None:
        return [*argv, *extra]
    provider, url = route
    # Pin the validated route so later config file changes cannot redirect these headers.
    argv += ["-c", "model_provider=" + json.dumps(provider), "-c", f"model_providers.{provider}.base_url=" + json.dumps(url)]
    project = args.project
    root, automatic = directory_project(directory)
    if not project:
        path = Path(args.map or "~/.config/cwr/projects.json").expanduser()
        project = automatic
        if args.map is not None or path.exists():
            if path.stat().st_size > 256 * 1024:
                raise ValueError("目录映射文件过大")
            data = json.loads(path.read_text(encoding="utf-8"))
            project = project_for(directory, data["projects"], fallback=automatic)
    if not isinstance(project, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", project):
        raise ValueError("项目标识只接受字母、数字及 . _ : -")
    argv += ["-c", f"model_providers.{provider}.http_headers.X-CWR-Project=" + json.dumps(project)]
    if project == automatic:
        # Only a short directory name is sent for display, never the full path.
        name = "".join(ch for ch in root.name if ch.isprintable()).encode("utf-8")[:64].decode("utf-8", errors="ignore")
        if name:
            argv += ["-c", f"model_providers.{provider}.http_headers.X-CWR-Project-Name=" + json.dumps(quote(name, safe=""))]
    return [*argv, *extra]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default=os.getcwd())
    parser.add_argument("--project")
    parser.add_argument("--map", help="可选目录映射；默认读取 ~/.config/cwr/projects.json（存在时），未匹配目录自动生成标识")
    parser.add_argument("--provider", help="要求检测到的 CPA 提供商 ID 与此值一致，不切换 Codex 配置")
    parser.add_argument("--codex", default="codex")
    parser.add_argument("--mode", choices=MODES, help="本次模式：auto 按配置与已登记地址添加项目头；cpa 要求检测成功；official 使用官方登录")
    action = parser.add_mutually_exclusive_group()
    action.add_argument("--setup-codeman", nargs="?", const="~/.codeman", metavar="DIRECTORY",
                        help="配置 Codeman 的 Codex 启动入口，默认 ~/.codeman；保留模型、推理强度及恢复参数")
    action.add_argument("--set-mode", choices=MODES, help="保存默认模式后退出；Codeman 下次启动 Codex 时自动使用")
    action.add_argument("--status", action="store_true", help="显示客户端模式及 CPA 提供商，不启动 Codex 或查询登录凭据")
    action.add_argument("--trust-cpa-url", metavar="URL", help="登记允许接收项目头的 CPA 完整 API 地址后退出；官方地址不可登记")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if (args.setup_codeman is not None or args.set_mode is not None or args.status or args.trust_cpa_url is not None) \
            and (args.arguments or args.mode is not None or args.provider is not None):
        parser.error("设置或状态命令不能同时指定启动模式、提供商或 Codex 参数")
    if args.setup_codeman is not None:
        try:
            path = configure_codeman(args.setup_codeman)
        except (ValueError, OSError, subprocess.TimeoutExpired) as error:
            parser.error(str(error))
        print("Codeman 的 Codex 入口已配置：" + str(path))
        print("重启 Codeman 服务后，Codex 会话会使用客户端保存的模式；模型、推理强度和恢复参数保留。")
        return 0
    try:
        settings = client_settings()
        if args.trust_cpa_url is not None:
            url = cpa_url(args.trust_cpa_url)
            urls = settings.get("cpa_urls", [])
            if url not in urls:
                if len(urls) >= 32:
                    raise ValueError("最多登记 32 个 CPA 地址")
                save_settings(cpa_urls=[*urls, url])
            print("已登记 CPA 地址：" + url)
            return 0
        if args.set_mode is not None:
            save_settings(mode=args.set_mode)
            labels = {"auto": "根据配置和已登记地址检测", "official": "官方 ChatGPT 登录", "cpa": "要求 CPA 项目调度"}
            print("已保存客户端模式：" + args.set_mode + "（" + labels[args.set_mode] + "）。")
            print("从 Codeman 下次新建或重新启动 Codex 会话时生效，正在运行的会话保持原模式。")
            return 0
        args.mode = args.mode or settings.get("mode", "auto")
        if args.status:
            print("客户端模式：" + args.mode)
            print("CPA 提供商：" + settings.get("provider", "按生效配置检测") + "（保存的 ID 仅约束 CPA 模式）")
            print("已登记 CPA 地址：" + (", ".join(settings.get("cpa_urls", [])) or "无（不会添加项目头）"))
            print("客户端设置：" + str(client_settings_path()))
            return 0
        if args.mode == "official":
            if args.provider is not None:
                raise ValueError("--provider 仅用于 CPA 模式；官方模式使用内置 openai 提供商")
            return run_codex(command(args), env=official_environment())[0]
        args.provider = args.provider or (settings.get("provider") if args.mode == "cpa" else None)
        args.cpa_urls = settings.get("cpa_urls", [])
        return run_codex(command(args))[0]
    except (ValueError, OSError, KeyError, TypeError, json.JSONDecodeError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    raise SystemExit(main())
