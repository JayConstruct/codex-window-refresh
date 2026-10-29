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
from urllib.parse import quote


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
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", args.provider):
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
    parser.add_argument("--provider", default="cpa", help="写入项目请求头的提供商 ID，默认 cpa；请填写 Codex 本次使用的 ID")
    parser.add_argument("--codex", default="codex")
    parser.add_argument("arguments", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    try:
        argv = command(args)
        os.execvp(argv[0], argv)
    except (ValueError, OSError, KeyError, TypeError, json.JSONDecodeError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
