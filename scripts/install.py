#!/usr/bin/env python3
"""Install the CLIProxyAPI window refresh plugin on Debian 12/13."""

import argparse
import copy
import datetime
import errno
import fcntl
import getpass
import grp
import hashlib
import ipaddress
import json
import os
import pathlib
import platform
import pwd
import re
import shutil
import socket
import ssl
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

try:
    import yaml
except ImportError:
    yaml = None


PLUGIN = "codex-window-refresh"
DEFAULT_VERSION = "latest"
REPOSITORY = "https://github.com/JayConstruct/codex-window-refresh"
RELEASE_API = "https://api.github.com/repos/JayConstruct/codex-window-refresh/releases"
VERSION_PATTERN = re.compile(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-rc\.([1-9]\d*))?")
ASSET_FILES = {
    "codex-window-refresh.so", "window-refresh", "config.example.yaml",
    "INSTALL.md", "PLUGIN.md", "RUNTIME.md", "NOTICE", "LICENSE.upstream",
    "BUILD-INFO.json", "install.py",
}


class InstallError(Exception):
    pass


class ManagementQueryError(InstallError):
    def __init__(self, message, status=None):
        super().__init__(message)
        self.status = status


def require(condition, message):
    if not condition:
        raise InstallError(message)


def run(*args):
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    require(result.returncode == 0, f"命令失败：{args[0]}；请检查对应服务或文件。")
    return result.stdout.strip()


def safe_path(path):
    path = pathlib.Path(os.path.abspath(os.path.expanduser(str(path))))
    require(not any(ord(c) < 32 or c in {'"', '\\', '%'} for c in str(path)),
            "安装路径含控制字符或 systemd 不支持的特殊字符，请使用普通绝对路径。")
    for component in [path, *path.parents]:
        require(not component.is_symlink(), f"安装目标包含符号链接，请使用真实路径：{component}")
    return path


def private_file(path):
    path = safe_path(path)
    info = path.stat()
    require(stat.S_ISREG(info.st_mode) and info.st_uid == 0 and info.st_mode & 0o077 == 0,
            f"文件须由 root 拥有且权限为 0600：{path}")
    return path


def load_yaml(text):
    require(yaml is not None, "缺少 YAML 解析器，请先执行：apt-get install python3-yaml")
    try:
        # Reject references rather than expanding unbounded aliases or rewriting anchors.
        require(not any(isinstance(t, (yaml.tokens.AliasToken, yaml.tokens.AnchorToken))
                        for t in yaml.scan(text)), "配置包含 YAML 锚点或别名，请改为普通映射后安装。")
        node = yaml.compose(text)
        require(isinstance(node, yaml.MappingNode) and not node.flow_style,
                "配置顶层必须是单个普通 YAML 映射。")

        def inspect(value):
            if isinstance(value, yaml.MappingNode):
                seen = set()
                for key, child in value.value:
                    require(isinstance(key, yaml.ScalarNode) and key.tag == "tag:yaml.org,2002:str",
                            "配置的映射键必须为字符串。")
                    require(key.value not in seen and key.value != "<<", "配置含重复键或合并键，请先修正。")
                    seen.add(key.value)
                    inspect(child)
            elif isinstance(value, yaml.SequenceNode):
                for child in value.value:
                    inspect(child)

        inspect(node)
        data = yaml.safe_load(text)
        return data, node
    except yaml.YAMLError:
        # Parser exceptions can include secret-containing source lines.
        raise InstallError("YAML 格式无效；为避免输出配置中的密钥，不显示原始解析内容。") from None


def replace_plugins(text, plugins):
    data, node = load_yaml(text)
    rendered = yaml.safe_dump({"plugins": plugins}, allow_unicode=True, sort_keys=False)
    pair = next(((key, value) for key, value in node.value if key.value == "plugins"), None)
    if pair:
        key, value = pair
        require(key.start_mark.column == 0, "plugins 必须位于配置顶层。")
        result = text[:key.start_mark.index] + rendered + text[value.end_mark.index:]
    else:
        end = next((t.start_mark.index for t in yaml.scan(text)
                    if isinstance(t, yaml.tokens.DocumentEndToken)), len(text))
        prefix = text[:end]
        result = prefix + ("" if prefix.endswith("\n") else "\n") + rendered + text[end:]
    actual, _ = load_yaml(result)
    expected = dict(data, plugins=plugins)
    require(actual == expected, "配置合并校验失败，没有写入 CPA 配置。")
    return result


def bridge_url(value):
    parsed = urllib.parse.urlsplit(value)
    require(parsed.scheme in {"http", "https"} and parsed.hostname and not parsed.username
            and not parsed.password and not parsed.query and not parsed.fragment,
            "管理地址须为无用户名、查询参数和片段的 HTTP(S) URL。")
    require(parsed.path.rstrip("/") in {"", "/v0/management", "/v0/management/api-call",
                                       "/v8/management", "/v8/management/requests/api-call"}, "管理地址的路径不受插件支持。")
    if parsed.path.rstrip("/") == "/v8/management":
        return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, "/v8/management/requests/api-call", "", ""))
    return value.rstrip("/")


def verify_elf(path, architecture):
    with path.open("rb") as file:
        header = file.read(20)
    machine = {"amd64": 62, "arm64": 183}[architecture]
    require(len(header) == 20 and header[:6] == b"\x7fELF\x02\x01"
            and struct.unpack_from("<H", header, 18)[0] == machine,
            f"程序 ELF 架构不匹配：{path.name}")


def detect_processes(binary):
    found = []
    for proc in pathlib.Path("/proc").iterdir():
        if not proc.name.isdigit():
            continue
        try:
            if (proc / "exe").resolve() == binary:
                found.append({"uid": proc.stat().st_uid, "cwd": (proc / "cwd").resolve(),
                              "args": (proc / "cmdline").read_bytes().decode().split("\0")[:-1]})
        except (OSError, UnicodeError):
            continue
    return found


def cli_config(args, cwd):
    found = []
    for index, argument in enumerate(args):
        if argument in {"-config", "--config"} and index + 1 < len(args):
            found.append(args[index + 1])
        elif argument.startswith(("-config=", "--config=")):
            found.append(argument.split("=", 1)[1])
    require(len(found) <= 1, "CPA 启动参数包含多个配置路径，请显式指定 --config。")
    if found:
        path = pathlib.Path(found[0])
        return path if path.is_absolute() else cwd / path
    return None


def service_info(name):
    if not name:
        return None
    require(re.fullmatch(r"[A-Za-z0-9_.@-]+(?:\.service)?", name) and not name.startswith("-"),
            "无效的 systemd 服务名称。")
    name = name if name.endswith(".service") else name + ".service"
    raw = run("systemctl", "show", name, "--property=LoadState,User,Group,WorkingDirectory,ExecStart")
    info = dict(line.split("=", 1) for line in raw.splitlines() if "=" in line)
    require(info.get("LoadState") == "loaded", "指定的 systemd 服务不存在或尚未加载。")
    info["name"] = name
    return info


def infer(args):
    if args.cpa_dir:
        directory = safe_path(args.cpa_dir)
    else:
        candidates = [pathlib.Path.cwd(), pathlib.Path("/root/cliproxyapi"), pathlib.Path("/opt/cpa")]
        directories = {p for p in candidates if any((p / n).is_file() for n in ["cli-proxy-api", "CLIProxyAPI"])}
        require(len(directories) == 1, "不能唯一确定 CPA 目录，请指定 --cpa-dir。")
        directory = safe_path(directories.pop())
    require(directory.is_dir(), "CPA 目录不存在。")
    binaries = [directory / n for n in ["cli-proxy-api", "CLIProxyAPI"] if (directory / n).is_file()]
    require(len(binaries) == 1, "CPA 目录中应有唯一的 cli-proxy-api 或 CLIProxyAPI 程序。")
    binary = binaries[0].resolve()
    processes = detect_processes(binary)
    require(len(processes) <= 1, "发现多个 CPA 实例，请分别安装到独立目录。")
    service = service_info(args.service)
    if service:
        require(str(binary) in service.get("ExecStart", ""), "服务 ExecStart 不包含目标 CPA 程序，请确认 --service。")
    user = args.user or (service.get("User") or "root" if service else None)
    if not user and processes:
        user = pwd.getpwuid(processes[0]["uid"]).pw_name
    require(user, "CPA 尚未运行，无法确定运行用户；请指定 --user 或 --service。")
    try:
        account = pwd.getpwnam(user)
        gid = grp.getgrnam(service["Group"]).gr_gid if service and service.get("Group") else account.pw_gid
    except KeyError:
        raise InstallError("指定的 CPA 用户或服务组不存在。") from None
    require(not service or not args.user or args.user == (service.get("User") or "root"),
            "--user 与 systemd 服务用户不一致。")
    if processes:
        require(processes[0]["uid"] == account.pw_uid, "运行中的 CPA 用户与指定用户不一致。")
    cwd = processes[0]["cwd"] if processes else pathlib.Path(service.get("WorkingDirectory") or "/") if service else None
    if args.working_dir:
        requested = safe_path(args.working_dir)
        require(cwd is None or requested == cwd, "--working-dir 与运行进程或服务的工作目录不一致。")
        cwd = requested
    observed_config = cli_config(processes[0]["args"], cwd) if processes else None
    if processes and observed_config is None:
        observed_config = cwd / "config.yaml"
    if args.config:
        config = safe_path(args.config)
        require(observed_config is None or safe_path(observed_config) == config,
                "--config 与运行中的 CPA 配置路径不一致。")
    else:
        config = observed_config
        # A custom service command may use a different config; do not silently guess it.
        require(config is not None or not service or not re.search(r"--?config(?:=|\s)", service.get("ExecStart", "")),
                "服务使用自定义配置路径，请指定 --config。")
        search = cwd if cwd is not None else directory
        candidates = [search / n for n in ["config.yaml", "config.yml"] if (search / n).is_file()]
        if config is None:
            require(len(candidates) == 1, "不能唯一确定主配置，请指定 --config。")
            config = candidates[0]
        config = safe_path(config)
    require(config.is_file(), "CPA 主配置文件不存在。")
    text = config.read_text()
    data, _ = load_yaml(text)
    plugins = data.get("plugins", {})
    require(isinstance(plugins, dict), "plugins 必须是映射。")
    configs = plugins.get("configs", {})
    require(isinstance(configs, dict), "plugins.configs 必须是映射。")
    wrapper = configs.get(PLUGIN, {})
    require(isinstance(wrapper, dict), "现有插件包装配置必须是映射。")
    require(not set(wrapper) - {"enabled", "priority", "config_file", "state_dir", "management_url", "management_key_env"},
            "现有插件含未支持的包装字段，请先按 docs/plugin.md 调整。")
    existing_dir = plugins.get("dir")
    require(existing_dir is None or isinstance(existing_dir, str), "plugins.dir 必须是路径字符串。")
    for name in ["config_file", "state_dir", "management_url", "management_key_env"]:
        require(name not in wrapper or isinstance(wrapper[name], str) and wrapper[name],
                f"现有插件 {name} 必须是非空字符串。")
    require("enabled" not in wrapper or isinstance(wrapper["enabled"], bool), "现有插件 enabled 必须是布尔值。")
    require("priority" not in wrapper or type(wrapper["priority"]) is int, "现有插件 priority 必须是整数。")

    def host_plugin_path(value):
        value = value.strip()
        if value.startswith("~"):
            require(value == "~" or value.startswith("~/"), "插件目录的 ~ 必须指 CPA 用户家目录。")
            return pathlib.Path(account.pw_dir) / value[2:] if value != "~" else pathlib.Path(account.pw_dir)
        return pathlib.Path(value)

    if args.plugin_dir:
        plugin_dir = safe_path(args.plugin_dir)
        if existing_dir:
            current = host_plugin_path(existing_dir)
            require(current.is_absolute() or cwd is not None, "已有相对插件目录，但无法核实 CPA 工作目录。")
            current = current if current.is_absolute() else cwd / current
            require(safe_path(current) == plugin_dir, "不能迁移现有插件目录；请使用原 plugins.dir。")
    elif existing_dir:
        require(isinstance(existing_dir, str), "plugins.dir 必须是路径字符串。")
        plugin_dir = host_plugin_path(existing_dir)
        require(plugin_dir.is_absolute() or cwd is not None,
                "已有相对 plugins.dir 且 CPA 未运行，请指定 --working-dir。")
        plugin_dir = safe_path(plugin_dir if plugin_dir.is_absolute() else cwd / plugin_dir)
    else:
        require(not configs or cwd is not None, "已有其他插件且未设置 plugins.dir，请指定 --working-dir 以保留默认插件目录。")
        plugin_dir = safe_path((cwd if cwd is not None else directory) / "plugins")
    policy = safe_path(wrapper.get("config_file") or directory / "window-refresh.yaml")
    state = safe_path(wrapper.get("state_dir") or directory / "window-refresh-state")
    require(not wrapper.get("config_file") or pathlib.Path(wrapper["config_file"]).is_absolute(), "现有策略路径必须为绝对路径。")
    require(not wrapper.get("state_dir") or pathlib.Path(wrapper["state_dir"]).is_absolute(), "现有状态路径必须为绝对路径。")
    server = data.get("server", {})
    require(isinstance(server, dict), "server 配置须为映射。")
    tls = server.get("tls", data.get("tls", {}))
    if not args.management_url and not wrapper.get("management_url"):
        require(not isinstance(tls, dict) or not tls.get("enable"), "CPA 已启用 TLS，请指定 --management-url。")
    port = server.get("port", data.get("port", 8317))
    require(isinstance(port, int) and 1 <= port <= 65535, "CPA 端口无效。")
    url = bridge_url(args.management_url or wrapper.get("management_url") or f"http://127.0.0.1:{port}/v0/management")
    env_name = wrapper.get("management_key_env", "CPA_MANAGEMENT_KEY")
    require(isinstance(env_name, str) and re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", env_name), "密钥环境变量名无效。")
    env = safe_path(directory / "window-refresh.env")
    record = safe_path(directory / "window-refresh-install")
    dropin = None
    if service:
        dropin = safe_path(pathlib.Path("/etc/systemd/system") / (service["name"] + ".d") / "60-window-refresh.conf")
    return {"directory": directory, "binary": binary, "config": config, "text": text, "data": data,
            "plugins": plugins, "wrapper": wrapper, "plugin_dir": plugin_dir, "policy": policy,
            "state": state, "env": env, "record": record, "dropin": dropin, "service": service,
            "uid": account.pw_uid, "gid": gid, "user": user, "url": url, "env_name": env_name}


def download(url, destination, limit):
    require(url.startswith("https://github.com/"), "仅从固定 GitHub Release 地址下载。")
    with urllib.request.urlopen(url, timeout=60) as response, destination.open("wb") as output:
        require(urllib.parse.urlsplit(response.url).scheme == "https", "下载重定向未使用 HTTPS。")
        size = 0
        while True:
            chunk = response.read(65536)
            if not chunk:
                break
            size += len(chunk)
            require(size <= limit, "下载文件超过安装限制。")
            output.write(chunk)


def version_key(version):
    match = VERSION_PATTERN.fullmatch(version) if isinstance(version, str) else None
    if not match:
        return None
    major, minor, patch, candidate = match.groups()
    return (int(major), int(minor), int(patch), candidate is None, int(candidate or 0))


def release_page(page):
    request = urllib.request.Request(
        f"{RELEASE_API}?per_page=100&page={page}",
        headers={"Accept": "application/vnd.github+json", "User-Agent": f"{PLUGIN}-installer",
                 "X-GitHub-Api-Version": "2022-11-28"})
    try:
        with urllib.request.urlopen(request, timeout=30) as result:
            address = urllib.parse.urlsplit(result.url)
            require(address.scheme == "https" and address.hostname == "api.github.com"
                    and not address.username and not address.password,
                    "版本查询返回非预期地址；可用 --version 指定版本。")
            raw = result.read((2 << 20) + 1)
            require(len(raw) <= 2 << 20, "版本查询响应过大；可用 --version 指定版本。")
        releases = json.loads(raw)
    except (OSError, ValueError):
        raise InstallError("无法查询 GitHub 发布版本（网络或 API 限流）；请稍后重试，或用 --version 指定版本。") from None
    require(isinstance(releases, list) and len(releases) <= 100
            and all(isinstance(item, dict) for item in releases),
            "GitHub 版本响应无效；可用 --version 指定版本。")
    return releases


def latest_version(architecture):
    # The /latest endpoint excludes prereleases. Compare supported version tags
    # numerically instead of trusting publication order or GitHub's Latest badge.
    candidates = []
    for page in range(1, 6):
        releases = release_page(page)
        for item in releases:
            key = version_key(item.get("tag_name"))
            if item.get("draft") is False and item.get("published_at") and key is not None:
                candidates.append((key, item))
        if len(releases) < 100:
            break
    else:
        raise InstallError("发布记录超过自动查询限制；请用 --version 指定版本。")
    require(candidates, "没有可安装的已发布正式版或 rc 测试版。")
    _, release = max(candidates, key=lambda entry: entry[0])
    version = release["tag_name"]
    assets = release.get("assets")
    require(isinstance(assets, list) and all(isinstance(asset, dict) for asset in assets),
            "最新发布版本的安装包信息无效。")
    for name, limit in [(f"{PLUGIN}_{version}_linux_{architecture}.tar.gz", 64 << 20),
                        ("SHA256SUMS", 65536)]:
        matches = [asset for asset in assets if asset.get("name") == name]
        require(len(matches) == 1 and matches[0].get("state") == "uploaded"
                and type(matches[0].get("size")) is int and 0 < matches[0]["size"] <= limit,
                f"最新版本 {version} 缺少有效的 {name}；请稍后重试或用 --version 指定版本。")
    return version


def verify_archive(archive, checksum_file, filename, output, version, architecture):
    matches = []
    for line in checksum_file.read_text().splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?([^\s]+)", line)
        if match and match[2] == filename:
            matches.append(match[1].lower())
    require(len(matches) == 1, "校验和文件缺失或重复了目标安装包。")
    require(archive.stat().st_size <= 64 << 20, "安装包过大。")
    require(hashlib.sha256(archive.read_bytes()).hexdigest() == matches[0], "SHA-256 校验失败，未安装文件。")
    prefix = filename[:-7]
    seen = set()
    total = 0
    try:
        with tarfile.open(archive, "r:gz") as tar:
            for member in tar:
                parts = pathlib.PurePosixPath(member.name).parts
                require(member.isfile() and len(parts) == 2 and parts[0] == prefix and parts[1] in ASSET_FILES,
                        "安装包包含非预期路径或文件类型。")
                require(parts[1] not in seen and 0 <= member.size <= 32 << 20, "安装包包含重复文件或超大文件。")
                total += member.size
                require(total <= 64 << 20, "解压内容超过安装限制。")
                seen.add(parts[1])
                content = tar.extractfile(member)
                require(content is not None, "无法读取安装包文件。")
                (output / parts[1]).write_bytes(content.read())
    except (tarfile.TarError, EOFError):
        raise InstallError("安装包无效。") from None
    require({"codex-window-refresh.so", "window-refresh", "config.example.yaml", "BUILD-INFO.json"} <= seen,
            "安装包缺少必要文件。")
    metadata = json.loads((output / "BUILD-INFO.json").read_text())
    require(metadata.get("release") == version and metadata.get("architecture") == architecture
            and metadata.get("platform") == "linux" and metadata.get("host_abi") == 1 and metadata.get("rpc_schema") == 6,
            "安装包版本、架构或 ABI 信息不匹配。")
    for name in ["codex-window-refresh.so", "window-refresh"]:
        verify_elf(output / name, architecture)
    glibc = metadata.get("minimum_glibc_symbols")
    local = platform.libc_ver()[1]
    require(glibc and local and tuple(map(int, local.split("."))) >= tuple(map(int, glibc.split("."))),
            "服务器 glibc 版本低于安装包要求。")
    (output / "window-refresh").chmod(0o755)


def atomic_write(path, data, mode, uid=0, gid=0):
    path = safe_path(path)
    require(path.parent.is_dir(), f"目标目录不存在：{path.parent}")
    fd, temporary = tempfile.mkstemp(prefix=".window-refresh-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as file:
            os.fchmod(file.fileno(), mode)
            os.fchown(file.fileno(), uid, gid)
            file.write(data)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


class Transaction:
    def __init__(self, backup):
        self.backup = backup
        self.originals = {}
        self.index = []

    def remember(self, path):
        path = safe_path(path)
        if path in self.originals:
            return
        if path.exists():
            require(path.is_file(), f"目标不是普通文件：{path}")
            info = path.stat()
            data = path.read_bytes()
            original = (data, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
            name = f"{len(self.originals):03d}-{path.name}"
            atomic_write(self.backup / name, data, 0o600)
            self.index.append({"path": str(path), "backup": name, "mode": original[1], "uid": info.st_uid, "gid": info.st_gid})
        else:
            original = None
            self.index.append({"path": str(path), "backup": None})
        self.originals[path] = original
        atomic_write(self.backup / "index.json", json.dumps(self.index, indent=2).encode(), 0o600)

    def write(self, path, data, mode, uid=0, gid=0):
        self.remember(path)
        atomic_write(path, data, mode, uid, gid)

    def remove(self, path):
        self.remember(path)
        if path.exists():
            path.unlink()

    def rollback(self):
        for path, original in reversed(list(self.originals.items())):
            if original is None:
                if path.exists():
                    path.unlink()
            else:
                atomic_write(path, *original)


def directory(path, mode, uid, gid, strict=False):
    path = safe_path(path)
    if path.exists():
        require(path.is_dir(), f"目标不是目录：{path}")
        if strict:
            info = path.stat()
            require(stat.S_IMODE(info.st_mode) == mode and info.st_uid == uid,
                    f"现有目录的拥有者或权限不符，未修改：{path}")
        return
    path.mkdir(parents=True, mode=mode)
    os.chown(path, uid, gid)
    path.chmod(mode)


def readable_by(path, uid, gid):
    if uid == 0:
        return
    user = pwd.getpwuid(uid).pw_name
    groups = set(os.getgrouplist(user, gid))
    for part in [path, *path.parents]:
        if not part.exists():
            continue
        info = part.stat()
        bits = info.st_mode >> (6 if info.st_uid == uid else 3 if info.st_gid in groups else 0)
        mask = 1 if part.is_dir() else 4
        require(bits & mask, f"CPA 运行用户无法访问：{part}；请使用可访问的安装目录。")


def environment_content(key, name):
    require(bool(key) and len(key) <= 4096 and all(32 <= ord(c) < 127 for c in key), "管理密钥须为非空、无换行的 ASCII 字符串。")
    # systemd EnvironmentFile uses double-quoted values without shell expansion.
    escaped = key.replace("\\", "\\\\").replace('"', '\\"')
    return f'{name}="{escaped}"\n'.encode()


def management_credentials(args, plan):
    if args.key_file:
        key = private_file(args.key_file).read_text().rstrip("\r\n")
        content = environment_content(key, plan["env_name"])
        if plan["env"].exists():
            private_file(plan["env"])
        return key, content
    if plan["env"].exists():
        private_file(plan["env"])
        saved = plan["env"].read_text()
        match = re.fullmatch(re.escape(plan["env_name"]) + r'="((?:[^"\n\\]|\\["\\])+)"\n?', saved)
        require(match, "现有环境文件不是安装器管理的格式；请用 --key-file 明确更新。")
        # Decode only escapes emitted by environment_content. Never evaluate
        # shell expressions or expand variables from the private environment file.
        key = re.sub(r'\\(["\\])', lambda m: m[1], match[1])
        environment_content(key, plan["env_name"])
        return key, None
    require(not args.yes and sys.stdin.isatty(),
            "首次安装请提供 root 拥有、0600 权限的 --key-file，或使用交互输入。")
    key = getpass.getpass("输入 CPA 网页面板管理密钥（原始密钥，非 API Key / bcrypt 哈希，不显示）：")
    return key, environment_content(key, plan["env_name"])


class NoManagementRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # Authorization must never follow an HTTP redirect, even to the same host.
        raise InstallError("管理接口发生重定向，请检查 --management-url。")


def model_discovery_base(url):
    parsed = urllib.parse.urlsplit(bridge_url(url))
    path = parsed.path.rstrip("/")
    prefix = "/v8/management" if path.startswith("/v8/") else "/v0/management"
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, prefix, "", ""))


def loopback_host(host):
    if host == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def management_json(base, route, key, timeout):
    require(route in {"/auth-files", "/credentials"} or
            route.startswith(("/auth-files/models?name=", "/credentials/models?name=")),
            "模型查询路径无效。")
    request = urllib.request.Request(base + route,
                                     headers={"Authorization": "Bearer " + key, "Accept": "application/json"})
    handlers = [NoManagementRedirect()]
    if loopback_host(urllib.parse.urlsplit(base).hostname):
        # Local management credentials must not pass through environment proxies.
        handlers.append(urllib.request.ProxyHandler({}))
    opener = urllib.request.build_opener(*handlers)
    endpoint = base + route.split("?", 1)[0]
    try:
        with opener.open(request, timeout=timeout) as response:
            require(response.url == request.full_url, "管理接口地址已改变，请检查 --management-url。")
            raw = response.read((4 << 20) + 1)
            require(len(raw) <= 4 << 20, "模型查询响应过大，请用 --model 指定。")
        value = json.loads(raw)
    except urllib.error.HTTPError as error:
        status = error.code
        error.close()
        reason = {
            401: "管理认证失败；请使用登录 CPA 网页面板的原始管理密钥，不是模型 API Key 或配置文件中的 bcrypt 哈希。",
            403: "管理访问被拒绝；检查远程访问限制、管理密钥配置及失败次数过多导致的 IP 封禁。",
            404: "管理接口不存在或未启用；检查地址、管理密钥配置及 CPA 版本。仅支持 v8 的入口可用 --management-url 指定 /v8/management。",
            429: "管理接口限流，请稍后重试。",
        }.get(status, "管理接口返回异常，请检查 CPA 日志及反向代理。")
        raise ManagementQueryError(f"CPA 模型目录查询失败：HTTP {status}；{reason} 查询地址：{endpoint}", status) from None
    except (urllib.error.URLError, OSError) as error:
        cause = error.reason if isinstance(error, urllib.error.URLError) else error
        if isinstance(cause, ssl.SSLError):
            reason = "TLS 连接或证书验证失败；检查 HTTP/HTTPS 配置及受信任证书。"
        elif isinstance(cause, (TimeoutError, socket.timeout)):
            reason = "连接或读取超时；检查 CPA 是否运行及管理端口。"
        elif isinstance(cause, socket.gaierror):
            reason = "管理主机名无法解析；检查管理地址。"
        elif getattr(cause, "errno", None) == errno.ECONNREFUSED:
            reason = "连接被拒绝；检查 CPA 是否运行、监听地址及端口，可用 --management-url 指定。"
        else:
            reason = "网络连接失败；检查 CPA 监听地址、端口、防火墙及代理。"
        raise ManagementQueryError(f"CPA 模型目录查询失败：{reason} 查询地址：{endpoint}") from None
    except ValueError:
        raise ManagementQueryError(f"CPA 模型目录响应不是有效 JSON；检查是否连接到网页或反向代理的错误页。查询地址：{endpoint}") from None
    require(isinstance(value, dict), "CPA 模型目录格式不受支持，请用 --model 指定。")
    return value


def discover_codex_model(url, key):
    base = model_discovery_base(url)
    # Registry reads only; do not execute a model request to test availability.
    deadline = time.monotonic() + 30

    def read(route):
        remaining = deadline - time.monotonic()
        require(remaining > 0, "自动识别模型超时，请用 --model 指定。")
        return management_json(base, route, key, min(5, remaining))

    account_route = "/credentials" if base.endswith("/v8/management") else "/auth-files"
    entries = read(account_route).get("files")
    require(isinstance(entries, list) and all(isinstance(e, dict) for e in entries),
            "CPA 账号目录格式不受支持，请用 --model 指定。")
    accounts = []
    for entry in entries:
        provider = entry.get("provider") or entry.get("type")
        if provider != "codex" or entry.get("disabled") is True or entry.get("status") == "disabled":
            continue
        # Include temporarily unavailable credentials: they can rejoin the pool.
        require(entry.get("disabled") is False,
                "CPA 账号启用状态不明确，请用 --model 指定。")
        name = entry.get("id") or entry.get("name")
        require(isinstance(name, str) and name and len(name.encode()) <= 4096,
                "CPA 账号标识不明确，请用 --model 指定。")
        require(name not in accounts, "CPA 账号目录包含重复标识，请用 --model 指定。")
        accounts.append(name)
    require(accounts, "没有已启用的 Codex 账号，请先在 CPA 登录账号，或用 --model 指定。")
    require(len(accounts) <= 64, "Codex 账号超过自动识别查询限制，请用 --model 指定。")
    common = None
    for name in accounts:
        models = read(account_route + "/models?" + urllib.parse.urlencode({"name": name})).get("models")
        require(isinstance(models, list) and all(isinstance(m, dict) for m in models),
                "CPA 模型目录格式不受支持，请用 --model 指定。")
        ids = {m["id"] for m in models if isinstance(m.get("id"), str)
               and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/():-]{0,255}", m["id"])
               and "REPLACE_WITH" not in m["id"].upper()}
        require(ids, "有 Codex 账号没有可用的模型目录，请用 --model 指定。")
        common = ids if common is None else common & ids
        require(common, "Codex 账号之间没有共同模型，请用 --model 指定并检查账号配置。")
    # Approval review models are registered by CPA but are not general wake-ups.
    common = {value for value in common if not re.search(r"(^|[-_/])auto[-_]review($|[-_/(])", value.lower())}
    require(common, "共同目录仅包含特殊审核模型，不适合自动唤醒；请用 --model 指定普通生成模型。")

    def preference(value):
        lowered = value.lower()
        for rank, variant in enumerate(["mini", "nano", "luna"]):
            if re.search(r"(^|[-_/])" + variant + r"($|[-_/(])", lowered):
                return rank, lowered, value
        return (3 if "codex" in lowered else 4), lowered, value

    # Only select a reported model; never guess an upstream model version.
    model = min(common, key=preference)
    return model, len(accounts)


def prompt(question, yes):
    if yes:
        return True
    require(sys.stdin.isatty(), "非交互运行请先 --dry-run 查看计划，再使用 --yes。")
    return input(question + " [y/N] ").strip().lower() in {"y", "yes"}


def install(args, plan, architecture):
    filename = f"{PLUGIN}_{args.version}_linux_{architecture}.tar.gz"
    with tempfile.TemporaryDirectory(prefix="window-refresh-install-") as temporary:
        temp = pathlib.Path(temporary)
        if args.package:
            require(args.checksums, "离线安装须提供 --checksums。")
            archive, checksums = pathlib.Path(args.package), pathlib.Path(args.checksums)
        else:
            archive, checksums = temp / filename, temp / "SHA256SUMS"
            base = f"{REPOSITORY}/releases/download/{args.version}"
            print("下载 Release 与 SHA-256 校验和……")
            download(base + "/" + filename, archive, 64 << 20)
            download(base + "/SHA256SUMS", checksums, 65536)
        unpacked = temp / "package"
        unpacked.mkdir()
        verify_archive(archive, checksums, filename, unpacked, args.version, architecture)
        policy = plan["policy"]
        credentials_loaded = False
        if policy.exists():
            require(policy.is_file(), "现有策略不是普通文件。")
            policy_bytes = policy.read_bytes()
            candidate = temp / ("policy" + policy.suffix)
        else:
            model = args.model
            if not model:
                key, key_bytes = management_credentials(args, plan)
                credentials_loaded = True
                print("从 CPA 已启用的 Codex 账号读取模型目录……")
                try:
                    model, count = discover_codex_model(plan["url"], key)
                    print(f"自动选择模型：{model}（{count} 个已启用 Codex 账号的共同模型；未发送测试请求）")
                except InstallError as error:
                    print(str(error))
                    require(not args.yes and sys.stdin.isatty(), "自动识别失败，请用 --model 指定实际支持的模型。")
                    model = input("CPA 实际支持的 Codex 模型名（手动填写）：").strip()
            require(model, "模型名不能为空，请用 --model 指定。")
            data, _ = load_yaml((unpacked / "config.example.yaml").read_text())
            data["enabled"] = False
            data["execution"]["model"] = model
            policy_bytes = yaml.safe_dump(data, allow_unicode=True, sort_keys=False).encode()
            candidate = temp / "policy.yaml"
        candidate.write_bytes(policy_bytes)
        run(str(unpacked / "window-refresh"), "check", str(candidate))
        if not credentials_loaded:
            key, key_bytes = management_credentials(args, plan)
        plugins = copy.deepcopy(plan["plugins"])
        plugins["enabled"] = True
        plugins["dir"] = str(plan["plugin_dir"])
        wrapper = dict(plan["wrapper"])
        wrapper.update({"config_file": str(policy), "state_dir": str(plan["state"]),
                        "management_url": plan["url"], "management_key_env": plan["env_name"]})
        wrapper.setdefault("enabled", True)
        wrapper.setdefault("priority", 0)
        plugins.setdefault("configs", {})[PLUGIN] = wrapper
        merged = replace_plugins(plan["text"], plugins).encode()
        require(prompt("安装上述文件并备份、更新 CPA plugins 配置？", args.yes), "已取消，未安装。")
        for target in [policy, plan["state"], plan["plugin_dir"], plan["config"]]:
            readable_by(target, plan["uid"], plan["gid"])
        for path in [policy.parent, plan["state"].parent]:
            require(path.is_dir(), f"策略或状态的父目录不存在：{path}")
        directory(plan["plugin_dir"], 0o755, 0, 0)
        directory(plan["state"], 0o700, plan["uid"], plan["gid"], strict=True)
        record_file = plan["record"] / "installed.json"
        if record_file.exists():
            previous = json.loads(private_file(record_file).read_text())
            require(previous.get("config") == str(plan["config"])
                    and previous.get("library") == str(plan["plugin_dir"] / "codex-window-refresh.so")
                    and previous.get("dropin") == (str(plan["dropin"]) if plan["dropin"] else None),
                    "升级时不能迁移配置、插件目录或 systemd 服务，请使用原安装参数。")
        backup = make_backup(plan)
        transaction = Transaction(backup)
        lib = plan["plugin_dir"] / "codex-window-refresh.so"
        checker = plan["directory"] / "window-refresh"
        try:
            require(plan["config"].read_text() == plan["text"], "CPA 主配置已被其他进程修改，请重新运行安装。")
            # A single stable ID may have multiple versioned files; do not load both.
            require(not list(plan["plugin_dir"].glob("codex-window-refresh-v*.so")),
                    "插件目录有同 ID 的版本化 .so，请先移到备份目录后安装。")
            transaction.write(lib, (unpacked / "codex-window-refresh.so").read_bytes(), 0o644)
            transaction.write(checker, (unpacked / "window-refresh").read_bytes(), 0o755)
            if not policy.exists():
                transaction.write(policy, policy_bytes, 0o600, plan["uid"], plan["gid"])
            if key_bytes is not None:
                transaction.write(plan["env"], key_bytes, 0o600)
            if plan["dropin"]:
                directory(plan["dropin"].parent, 0o755, 0, 0)
                body = f'# Managed by codex-window-refresh installer\n[Service]\nEnvironmentFile="{plan["env"]}"\n'
                transaction.write(plan["dropin"], body.encode(), 0o644)
            info = plan["config"].stat()
            transaction.write(plan["config"], merged, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
            hashes = {str(lib): hashlib.sha256(lib.read_bytes()).hexdigest(),
                      str(checker): hashlib.sha256(checker.read_bytes()).hexdigest()}
            if plan["dropin"]:
                hashes[str(plan["dropin"])] = hashlib.sha256(plan["dropin"].read_bytes()).hexdigest()
            record = {"version": args.version, "config": str(plan["config"]), "library": str(lib),
                      "checker": str(checker), "dropin": str(plan["dropin"]) if plan["dropin"] else None,
                      "service": plan["service"]["name"] if plan["service"] else None,
                      "backup": str(backup), "installed_hashes": hashes}
            transaction.write(plan["record"] / "installed.json", json.dumps(record, indent=2).encode(), 0o600)
            if plan["service"]:
                run("systemctl", "daemon-reload")
        except BaseException:
            transaction.rollback()
            if plan["service"]:
                try:
                    run("systemctl", "daemon-reload")
                except InstallError:
                    pass
            raise
        print(f"已安装 {args.version}。备份目录：{backup}")
        print("现有策略与状态已保留；新策略关闭自动请求。")
        finish(args, plan)


def make_backup(plan):
    directory(plan["record"], 0o700, 0, 0, strict=True)
    name = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S")
    return pathlib.Path(tempfile.mkdtemp(prefix=name + "-", dir=plan["record"]))


def finish(args, plan):
    if plan["service"]:
        service = plan["service"]["name"]
        if args.restart or (not args.yes and sys.stdin.isatty() and prompt(f"现在重启 {service}？", False)):
            run("systemctl", "restart", service)
            require(run("systemctl", "is-active", service) == "active", "CPA 重启后未处于 active 状态，请检查服务日志。")
            print("CPA 服务已重启。请通过管理认证检查插件 /summary 和 /capabilities。")
        else:
            print(f"尚未重启服务；生效命令：systemctl restart {service}")
    else:
        require(not args.restart, "--restart 仅支持明确指定的 systemd 服务。")
        if args.uninstall:
            print("请按原启动方式重启 CPA，使卸载生效。")
        else:
            print(f"请将 {plan['env']} 中的环境变量设置到 CPA 的启动环境，再按原启动方式重启。")
            print("该环境文件按 systemd 语法保存，请勿直接用 shell source 执行。")


def uninstall(args, plan):
    manifest = private_file(plan["record"] / "installed.json")
    data = json.loads(manifest.read_text())
    expected = {"config": plan["config"], "library": plan["plugin_dir"] / "codex-window-refresh.so",
                "checker": plan["directory"] / "window-refresh", "dropin": plan["dropin"]}
    require(all(data.get(name) == (str(path) if path else None) for name, path in expected.items()),
            "安装记录与当前路径或服务不一致，请使用原安装参数。")
    for name in ["library", "checker", "dropin"]:
        path = expected[name]
        if path is None:
            continue
        require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == data["installed_hashes"][str(path)],
                f"已安装文件被修改，未自动删除：{path}")
    plugins = copy.deepcopy(plan["plugins"])
    plugins.get("configs", {}).pop(PLUGIN, None)
    merged = replace_plugins(plan["text"], plugins).encode()
    require(prompt("移除插件配置、二进制和本脚本的 systemd override？策略、状态、环境文件与备份会保留。", args.yes), "已取消。")
    transaction = Transaction(make_backup(plan))
    try:
        require(plan["config"].read_text() == plan["text"], "CPA 主配置已改变，请重新运行。")
        info = plan["config"].stat()
        transaction.write(plan["config"], merged, stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
        for name in ["library", "checker", "dropin"]:
            path = expected[name]
            if path:
                transaction.remove(path)
        transaction.remove(manifest)
        if plan["service"]:
            run("systemctl", "daemon-reload")
    except BaseException:
        transaction.rollback()
        if plan["service"]:
            try:
                run("systemctl", "daemon-reload")
            except InstallError:
                pass
        raise
    print("插件已卸载；策略、窗口状态、环境文件和备份已保留。")
    finish(args, plan)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-dir", help="CPA 二进制所在目录，例如 /root/cliproxyapi")
    parser.add_argument("--config", help="CPA 主配置路径；默认检测运行参数或唯一 config.yaml/yml")
    parser.add_argument("--plugin-dir", help="插件目录；保留已有 plugins.dir")
    parser.add_argument("--working-dir", help="CPA 工作目录；运行中或 systemd 服务优先自动检测")
    parser.add_argument("--user", help="CPA 运行用户；优先检测运行进程或指定服务")
    parser.add_argument("--service", help="实际 systemd 服务名，不自动猜测")
    parser.add_argument("--management-url", help="管理入口；默认检测端口并使用本机 HTTP")
    parser.add_argument("--model", help="首次安装指定 Codex 模型；省略时自动读取已启用 Codex 账号的共同模型")
    parser.add_argument("--version", default=DEFAULT_VERSION,
                        help="默认 latest：最新已发布正式版或 rc 测试版；也可指定 vX.Y.Z 或 vX.Y.Z-rc.N")
    parser.add_argument("--key-file", help="仅 root 可读的纯文本管理密钥文件；不通过命令行传密钥")
    parser.add_argument("--package", help="离线安装包")
    parser.add_argument("--checksums", help="离线 SHA256SUMS 文件")
    parser.add_argument("--dry-run", action="store_true", help="仅检查和显示路径，不下载或修改文件")
    parser.add_argument("--check-model", action="store_true", help="只查询 CPA 模型并诊断管理连接；不下载、写入安装文件或重启")
    parser.add_argument("--yes", action="store_true", help="非交互安装，首次安装需 --key-file；自动识别失败须 --model")
    parser.add_argument("--restart", action="store_true", help="安装后重启明确指定的 systemd 服务")
    parser.add_argument("--uninstall", action="store_true", help="卸载本脚本安装的插件，保留策略和状态")
    args = parser.parse_args()
    require(os.geteuid() == 0, "请以 root 运行（sudo python3 install.py）。")
    require(platform.system() == "Linux", "当前安装器仅支持 Linux Debian 12/13。")
    architecture = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(architecture, "仅支持 amd64 和 arm64。")
    release = pathlib.Path("/etc/os-release").read_text()
    require(re.search(r'^ID=["\x27]?debian["\x27]?$', release, re.M)
            and re.search(r'^VERSION_ID=["\x27]?(12|13)["\x27]?$', release, re.M),
            "当前自动安装仅支持 Debian 12/13；其他系统请按 docs/install.md 手动安装。")
    require(args.version == "latest" or version_key(args.version) is not None,
            "版本须为 latest、vX.Y.Z 或 vX.Y.Z-rc.N。")
    require(args.uninstall or not args.package or args.version != "latest",
            "离线安装须使用 --version 指定安装包的准确版本。")
    require(not args.restart or args.service, "--restart 需要 --service。")
    require(not args.check_model or not (args.dry_run or args.uninstall or args.restart),
            "--check-model 不能与 --dry-run、--uninstall 或 --restart 同时使用。")
    plan = infer(args)
    verify_elf(plan["binary"], architecture)
    target = "最新发布版本（含 rc 测试版，执行时查询）" if args.version == "latest" else args.version
    operation = "模型诊断" if args.check_model else "卸载" if args.uninstall else "安装/升级"
    print(f"操作：{operation} / linux_{architecture}"
          + ("" if args.uninstall or args.check_model else f" / {target}"))
    for key in ["binary", "config", "plugin_dir", "policy", "state", "env"]:
        print(f"{key}: {plan[key]}")
    print(f"CPA 用户：{plan['user']}；systemd 服务：{plan['service']['name'] if plan['service'] else '未指定'}")
    print(f"管理入口：{plan['url']}；密钥将只保存到 root 拥有的 0600 环境文件。")
    if not args.check_model:
        print("安装时开启宿主 plugins.enabled；首次策略保持关闭。plugins 节的注释可能重新排版，原配置会备份。")
    if args.dry_run:
        print("仅预览，未下载、写入或重启。")
        return
    if args.check_model:
        key, _ = management_credentials(args, plan)
        model, count = discover_codex_model(plan["url"], key)
        print(f"自动选择模型：{model}（{count} 个已启用 Codex 账号的共同模型；未发送测试请求）")
        print("模型查询完成，未下载、写入安装文件或重启。")
        return
    if not args.uninstall and args.version == "latest":
        args.version = latest_version(architecture)
        print(f"本次安装版本：{args.version}（已固定，下载期间不再切换版本）")
    lock_path = safe_path(plan["directory"] / ".window-refresh-install.lock")
    with os.fdopen(os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600), "r+") as lock:
        require(os.fstat(lock.fileno()).st_uid == 0, "安装锁必须由 root 拥有。")
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.uninstall:
            uninstall(args, plan)
        else:
            install(args, plan, architecture)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("已中止。", file=sys.stderr)
        sys.exit(130)
    except (InstallError, OSError, ValueError, KeyError):
        # Do not expose exception contexts containing configuration or credentials.
        error = sys.exc_info()[1]
        print(str(error) if isinstance(error, InstallError) else "安装失败，请检查路径、权限、网络和安装包。", file=sys.stderr)
        sys.exit(1)
