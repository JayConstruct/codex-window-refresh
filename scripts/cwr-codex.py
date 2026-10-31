#!/usr/bin/env python3
"""Launch Codex with a project request header for the specified provider."""
import argparse
import hashlib
import json
import ntpath
import os
import posixpath
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
from urllib.parse import quote

DEFAULT_PROVIDER = "cliproxyapi"

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


def saved_provider():
    try:
        path = client_settings_path()
        if not path.exists():
            return None
        if path.stat().st_size > 4096:
            raise ValueError("客户端设置文件过大")
        data = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(data, dict) or not valid_provider(data.get("provider")):
            raise ValueError("客户端提供商 ID 无效")
        return data["provider"]
    except (OSError, ValueError):
        print("cwr-codex：无法读取已保存的提供商设置，本次使用默认 cliproxyapi。", file=sys.stderr)
        return None


def save_provider(provider):
    path = client_settings_path()
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    staged = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=".client-", delete=False) as stream:
            staged = Path(stream.name)
            json.dump({"provider": provider}, stream, ensure_ascii=False)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(staged, path)
    finally:
        if staged is not None:
            staged.unlink(missing_ok=True)


def run_codex(argv):
    """Preserve stdin/stdout terminals; relay and retain only bounded stderr."""
    tail = bytearray()
    process = subprocess.Popen(argv, stderr=subprocess.PIPE, bufsize=0)

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


def missing_provider(error, provider):
    clean = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", error)
    return re.search(r"model_providers\." + re.escape(provider) + r"\s*:\s*provider name must not be empty", clean) is not None


def ask_provider(current):
    print("\ncwr-codex：Codex 无法加载提供商“" + current + "”的定义。", file=sys.stderr)
    print("请填写 Codex 配置中实际使用的提供商 ID；此选择会保存到 " + str(client_settings_path()) + "。", file=sys.stderr)
    while True:
        try:
            print("提供商 ID（回车取消）：", end="", file=sys.stderr, flush=True)
            provider = input().strip()
        except (EOFError, KeyboardInterrupt):
            print(file=sys.stderr)
            return None
        if not provider:
            return None
        if valid_provider(provider):
            return provider
        print("只接受 1–64 个字母、数字、下划线或连字符，请重新输入。", file=sys.stderr)


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
    value_options = {"-c", "--config", "-p", "--profile", "-m", "--model", "-s", "--sandbox",
                     "-a", "--ask-for-approval", "-i", "--image", "--remote", "--remote-auth-token-env",
                     "--local-provider", "--add-dir", "--enable", "--disable", "-o", "--output-last-message",
                     "--output-schema"}
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
            if value in value_options and index + 1 < len(arguments):
                index += 1
                extra.append(arguments[index])
        index += 1
    return Path(directory if directory is not None else fallback).expanduser().resolve(strict=True), extra


def command(args):
    extra = args.arguments[1:] if args.arguments[:1] == ["--"] else args.arguments
    directory, extra = launch_directory(extra, args.directory)
    if not directory.is_dir():
        raise ValueError("启动目录必须是文件夹")
    if not valid_provider(args.provider):
        raise ValueError("调度提供商 ID 无效")
    argv = [args.codex, "-C", str(directory)]
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
    argv += ["-c", f"model_providers.{args.provider}.http_headers.X-CWR-Project=" + json.dumps(project)]
    if project == automatic:
        # Only a short directory name is sent for display, never the full path.
        name = "".join(ch for ch in root.name if ch.isprintable()).encode("utf-8")[:64].decode("utf-8", errors="ignore")
        if name:
            argv += ["-c", f"model_providers.{args.provider}.http_headers.X-CWR-Project-Name=" + json.dumps(quote(name, safe=""))]
    return [*argv, *extra]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default=os.getcwd())
    parser.add_argument("--project")
    parser.add_argument("--map", help="可选目录映射；默认读取 ~/.config/cwr/projects.json（存在时），未匹配目录自动生成标识")
    parser.add_argument("--provider", help="本次使用的提供商 ID；默认使用已记住的 ID 或 cliproxyapi，配置报错时可输入并记住")
    parser.add_argument("--codex", default="codex")
    parser.add_argument("--setup-codeman", nargs="?", const="~/.codeman", metavar="DIRECTORY",
                        help="配置 Codeman 的 Codex 启动入口，默认 ~/.codeman；保留模型、推理强度及恢复参数")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.setup_codeman is not None:
        try:
            path = configure_codeman(args.setup_codeman)
        except (ValueError, OSError, subprocess.TimeoutExpired) as error:
            parser.error(str(error))
        print("Codeman 的 Codex 入口已配置：" + str(path))
        print("重启 Codeman 服务后，新建及恢复的 Codex 会话会自动携带项目标识。")
        return 0
    args.provider = args.provider or saved_provider() or DEFAULT_PROVIDER
    try:
        while True:
            code, error = run_codex(command(args))
            if code != 1 or not missing_provider(error, args.provider):
                return code
            if not sys.stdin.isatty():
                print("cwr-codex：请使用 --provider 指定 Codex 中已有的提供商 ID，或在终端直接运行后按提示输入。", file=sys.stderr)
                return code
            provider = ask_provider(args.provider)
            if provider is None:
                return code
            save_provider(provider)
            args.provider = provider
            print("已记住提供商“" + provider + "”，正在启动 Codex。", file=sys.stderr)
    except (ValueError, OSError, KeyError, TypeError, json.JSONDecodeError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    raise SystemExit(main())
