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
import signal
import subprocess
import sys
import tempfile
import threading
from urllib.parse import quote

DEFAULT_PROVIDER = "cliproxyapi"


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


def command(args):
    directory = Path(args.directory).expanduser().resolve(strict=True)
    if not directory.is_dir():
        raise ValueError("启动目录必须是文件夹")
    if not valid_provider(args.provider):
        raise ValueError("调度提供商 ID 无效")
    extra = args.arguments[1:] if args.arguments[:1] == ["--"] else args.arguments
    if any(value in ("-C", "--cd") or value.startswith("--cd=") or value.startswith("-C") for value in extra):
        raise ValueError("请使用启动器的 --directory 指定目录")
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
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
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
