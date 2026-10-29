#!/usr/bin/env python3
"""Launch Codex with a stable CPA project routing label selected by directory."""
import argparse
import json
import os
from pathlib import Path
import re
import sys


def project_for(directory, mappings):
    directory = Path(directory).resolve()
    matches = []
    roots = set()
    for item in mappings:
        root = Path(item["directory"]).expanduser()
        if not root.is_absolute():
            raise ValueError("目录映射必须使用绝对路径")
        root = root.resolve()
        if root in roots:
            raise ValueError("目录映射重复")
        roots.add(root)
        if directory == root or root in directory.parents:
            matches.append((len(root.parts), item["project"]))
    if not matches:
        raise ValueError("当前目录没有项目映射，请指定 --project 或配置 --map")
    return max(matches)[1]


def command(args):
    directory = Path(args.directory).expanduser().resolve(strict=True)
    if not directory.is_dir():
        raise ValueError("启动目录必须是文件夹")
    project = args.project
    if not project:
        path = Path(args.map).expanduser()
        if path.stat().st_size > 256 * 1024:
            raise ValueError("目录映射文件过大")
        data = json.loads(path.read_text())
        project = project_for(directory, data["projects"])
    if not isinstance(project, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", project):
        raise ValueError("项目标识只接受字母、数字及 . _ : -")
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", args.provider):
        raise ValueError("provider 名称无效")
    extra = args.arguments[1:] if args.arguments[:1] == ["--"] else args.arguments
    if any(value in ("-C", "--cd") or value.startswith("--cd=") or value.startswith("-C") for value in extra):
        raise ValueError("请使用启动器的 --directory 指定目录")
    return [args.codex, "-C", str(directory), "-c", "model_provider=" + json.dumps(args.provider),
            "-c", f"model_providers.{args.provider}.http_headers.X-CWR-Project=" + json.dumps(project), *extra]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default=os.getcwd())
    parser.add_argument("--project")
    parser.add_argument("--map", default="~/.config/cwr/projects.json")
    parser.add_argument("--provider", default="cpa")
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
