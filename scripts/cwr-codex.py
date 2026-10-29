#!/usr/bin/env python3
"""Add a project label only when Codex is configured for the chosen CPA provider."""
import argparse
import json
import ntpath
import os
import posixpath
from pathlib import Path
import re
import sys

try:
    import tomllib
except ImportError:  # Python 3.10 clients can install the TOML backport.
    try:
        import tomli as tomllib
    except ImportError:
        tomllib = None


def read_config(path, optional=False):
    """Read configuration without exposing TOML contents in diagnostics."""
    try:
        with path.open("rb") as stream:
            raw = stream.read(1024 * 1024 + 1)
    except FileNotFoundError:
        if optional:
            return {}
        raise ValueError("找不到所选 profile 配置文件") from None
    except OSError:
        raise ValueError("无法读取 Codex 配置文件") from None
    if len(raw) > 1024 * 1024:
        raise ValueError("Codex 配置文件超过 1 MB")
    if tomllib is None:
        raise ValueError("读取配置需要 Python 3.11+，或为旧版 Python 安装 tomli")
    try:
        return tomllib.loads(raw.decode("utf-8"))
    except (ValueError, UnicodeError):
        raise ValueError("Codex 配置文件不是有效的 UTF-8 TOML") from None


def provider_options(arguments):
    """Observe provider-related CLI options; leave the original arguments intact."""
    profile, overrides = None, []
    index = 0
    while index < len(arguments):
        argument = arguments[index]
        if argument == "--":
            break  # Everything after this belongs to the prompt/command.
        if argument == "--oss" or argument.startswith("--oss=") or argument == "--remote" or argument.startswith("--remote="):
            return None
        value = None
        if argument in ("-c", "--config", "-p", "--profile"):
            if index + 1 >= len(arguments):
                return None
            index += 1
            value = arguments[index]
        elif argument.startswith("--config=") or argument.startswith("--profile="):
            value = argument.split("=", 1)[1]
        elif argument.startswith("-c") and len(argument) > 2:
            value = argument[2:]
        elif argument.startswith("-p") and len(argument) > 2:
            value = argument[2:]
        if value is not None:
            if argument.startswith(("-p", "--profile")):
                if profile is not None:
                    return None
                profile = value
            else:
                overrides.append(value)
        index += 1
    return profile, overrides


def routing_enabled(provider, arguments):
    """Gate routing on user/profile configuration, never select a provider."""
    options = provider_options(arguments)
    if options is None:
        return False
    profile, overrides = options
    config_home = Path(os.environ.get("CODEX_HOME") or "~/.codex").expanduser()
    try:
        config = read_config(config_home / "config.toml", optional=True)
        configured = config.get("model_provider")
        if profile is not None:
            if not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", profile) or profile in (".", ".."):
                return False
            selected = read_config(config_home / (profile + ".config.toml"))
            configured = selected.get("model_provider", configured)
        elif "profile" in config:
            # Legacy implicit profiles use a different configuration contract.
            # Let that Codex version resolve it without adding routing metadata.
            return False
        if configured != provider:
            return False
        # A CLI override may switch away from the provider selected in the file.
        # It cannot opt an unrelated configuration into project routing.
        effective = configured
        for override in overrides:
            key, separator, raw_value = override.partition("=")
            if not separator:
                return False
            if tomllib is None:
                raise ValueError("读取配置需要 Python 3.11+，或为旧版 Python 安装 tomli")
            try:
                key_fields = tomllib.loads(key.strip() + ' = ""')
            except ValueError:
                return False
            if "model_provider" not in key_fields:
                continue
            if key_fields["model_provider"] != "":
                return False
            try:
                value = tomllib.loads("value = " + raw_value)["value"]
            except ValueError:
                value = raw_value.strip()  # Codex accepts unquoted string overrides.
            effective = value
        return effective == provider
    except ValueError as error:
        print("cwr-codex：" + str(error) + "，本次跳过项目调度。", file=sys.stderr)
        return False


def project_for(directory, mappings):
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
    if not routing_enabled(args.provider, extra):
        return [*argv, *extra]
    project = args.project
    if not project:
        path = Path(args.map).expanduser()
        if path.stat().st_size > 256 * 1024:
            raise ValueError("目录映射文件过大")
        data = json.loads(path.read_text(encoding="utf-8"))
        project = project_for(directory, data["projects"])
    if not isinstance(project, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", project):
        raise ValueError("项目标识只接受字母、数字及 . _ : -")
    return [*argv, "-c", f"model_providers.{args.provider}.http_headers.X-CWR-Project=" + json.dumps(project), *extra]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default=os.getcwd())
    parser.add_argument("--project")
    parser.add_argument("--map", default="~/.config/cwr/projects.json")
    parser.add_argument("--provider", default="cpa", help="仅配置中的当前提供商与此 ID 匹配时添加项目标识，默认 cpa；不切换提供商")
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
