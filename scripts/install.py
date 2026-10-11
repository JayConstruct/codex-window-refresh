#!/usr/bin/env python3
"""Install the CLIProxyAPI window refresh plugin on Debian 12/13."""

import argparse
import copy
import datetime
import fcntl
import getpass
import grp
import hashlib
import json
import os
import pathlib
import platform
import pwd
import re
import shutil
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request

try:
    import yaml
except ImportError:
    yaml = None


PLUGIN = "codex-window-refresh"
DEFAULT_VERSION = "v0.1.0-rc.1"
REPOSITORY = "https://github.com/JayConstruct/codex-window-refresh"
ASSET_FILES = {
    "codex-window-refresh.so", "window-refresh", "config.example.yaml",
    "INSTALL.md", "PLUGIN.md", "RUNTIME.md", "NOTICE", "LICENSE.upstream",
    "BUILD-INFO.json", "install.py",
}


class InstallError(Exception):
    pass


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
                                       "/v8/management/requests/api-call"}, "管理地址的路径不受插件支持。")
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
        if policy.exists():
            require(policy.is_file(), "现有策略不是普通文件。")
            policy_bytes = policy.read_bytes()
            candidate = temp / ("policy" + policy.suffix)
        else:
            model = args.model
            if not model and not args.yes:
                require(sys.stdin.isatty(), "首次安装请提供 --model。")
                model = input("CPA 实际支持的 Codex 模型名：").strip()
            require(model, "首次安装请提供 --model，脚本不会猜测可用模型。")
            data, _ = load_yaml((unpacked / "config.example.yaml").read_text())
            data["enabled"] = False
            data["execution"]["model"] = model
            policy_bytes = yaml.safe_dump(data, allow_unicode=True, sort_keys=False).encode()
            candidate = temp / "policy.yaml"
        candidate.write_bytes(policy_bytes)
        run(str(unpacked / "window-refresh"), "check", str(candidate))
        key_bytes = None
        if args.key_file:
            key = private_file(args.key_file).read_text().rstrip("\r\n")
            key_bytes = environment_content(key, plan["env_name"])
            if plan["env"].exists():
                private_file(plan["env"])
        elif plan["env"].exists():
            private_file(plan["env"])
            require(re.fullmatch(re.escape(plan["env_name"]) + r'="(?:[^"\n\\]|\\.)+"\n?', plan["env"].read_text()),
                    "现有环境文件不是安装器管理的格式；请用 --key-file 明确更新。")
        else:
            require(not args.yes and sys.stdin.isatty(), "首次安装请提供 root 拥有、0600 权限的 --key-file，或使用交互输入。")
            key_bytes = environment_content(getpass.getpass("输入现有 CPA 管理密钥（不显示）："), plan["env_name"])
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
    parser.add_argument("--model", help="首次安装使用的可用 Codex 模型名")
    parser.add_argument("--version", default=DEFAULT_VERSION, help="固定的 Release candidate 版本")
    parser.add_argument("--key-file", help="仅 root 可读的纯文本管理密钥文件；不通过命令行传密钥")
    parser.add_argument("--package", help="离线安装包")
    parser.add_argument("--checksums", help="离线 SHA256SUMS 文件")
    parser.add_argument("--dry-run", action="store_true", help="仅检查和显示路径，不下载或修改文件")
    parser.add_argument("--yes", action="store_true", help="非交互安装，首次安装需 --model 和 --key-file")
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
    require(re.fullmatch(r"v\d+\.\d+\.\d+-rc\.[1-9]\d*", args.version), "版本须为固定 rc 版本。")
    require(not args.restart or args.service, "--restart 需要 --service。")
    plan = infer(args)
    verify_elf(plan["binary"], architecture)
    print(f"操作：{'卸载' if args.uninstall else '安装/升级'} {args.version} / linux_{architecture}")
    for key in ["binary", "config", "plugin_dir", "policy", "state", "env"]:
        print(f"{key}: {plan[key]}")
    print(f"CPA 用户：{plan['user']}；systemd 服务：{plan['service']['name'] if plan['service'] else '未指定'}")
    print(f"管理入口：{plan['url']}；密钥将只保存到 root 拥有的 0600 环境文件。")
    print("安装时开启宿主 plugins.enabled；首次策略保持关闭。plugins 节的注释可能重新排版，原配置会备份。")
    if args.dry_run:
        print("仅预览，未下载、写入或重启。")
        return
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
