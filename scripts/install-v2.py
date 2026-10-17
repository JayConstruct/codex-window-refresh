#!/usr/bin/env python3
"""Install/update the independent V2 backend for an existing CPA service."""
import argparse
import copy
import contextlib
import fcntl
import hashlib
import http.client
import importlib.util
import json
import os
import pathlib
import platform
import pwd
import re
import shutil
import socket
import sqlite3
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse

spec = importlib.util.spec_from_file_location("cwr_installer_common", pathlib.Path(__file__).with_name("install.py"))
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)
require, InstallError = common.require, common.InstallError
PREFIX = "codex-window-refresh-v2"
FILES = {"codex-window-refresh-v2-bridge.so", "window-refreshd", "BUILD-INFO.json",
         "install.py", "install-v2.py", "INSTALL.md", "PROTOTYPE.md", "NOTICE", "LICENSE.upstream"}


def latest(arch):
    candidates = []
    for page in range(1, 6):
        records = common.release_page(page)
        for item in records:
            tag = item.get("tag_name", "")
            if (re.fullmatch(r"v0\.2\.\d+-rc\.[1-9]\d*", tag) and item.get("draft") is False
                    and item.get("published_at")):
                candidates.append(item)
        if len(records) < 100:
            break
    else:
        raise InstallError("发布列表过长；请用 --version 固定 v2 版本。")
    require(candidates, "尚无已发布的 v2 测试版。")
    release = max(candidates, key=lambda r: common.version_key(r["tag_name"]))
    tag = release["tag_name"]
    for name, limit in [(f"{PREFIX}_{tag}_linux_{arch}.tar.gz", 64 << 20), ("SHA256SUMS", 65536)]:
        assets = [a for a in release.get("assets", []) if a.get("name") == name]
        require(len(assets) == 1 and assets[0].get("state") == "uploaded"
                and type(assets[0].get("size")) is int and 0 < assets[0]["size"] <= limit,
                f"v2 版本 {tag} 的 {name} 尚未完整发布。")
    return tag


def unpack(archive, checksums, filename, output, version, arch):
    require(archive.stat().st_size <= 64 << 20 and checksums.stat().st_size <= 65536, "安装文件过大。")
    matches = [m[1].lower() for line in checksums.read_text().splitlines()
               if (m := re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?([^\s]+)", line)) and m[2] == filename]
    require(len(matches) == 1 and hashlib.sha256(archive.read_bytes()).hexdigest() == matches[0], "安装包 SHA-256 校验失败。")
    seen, total = set(), 0
    with tarfile.open(archive, "r:gz") as tar:
        for member in tar:
            parts = pathlib.PurePosixPath(member.name).parts
            require(member.isfile() and len(parts) == 2 and parts[0] == filename[:-7]
                    and parts[1] in FILES and parts[1] not in seen and 0 <= member.size <= 32 << 20,
                    "安装包含非预期路径、重复文件或文件类型。")
            total += member.size
            require(total <= 64 << 20, "解压内容过大。")
            seen.add(parts[1])
            (output / parts[1]).write_bytes(tar.extractfile(member).read())
    require({"codex-window-refresh-v2-bridge.so", "window-refreshd", "BUILD-INFO.json"} <= seen, "安装包不完整。")
    info = json.loads((output / "BUILD-INFO.json").read_text())
    require(info.get("backend") == "v2" and info.get("release") == version and info.get("architecture") == arch
            and info.get("platform") == "linux" and info.get("host_abi") == 1 and info.get("rpc_schema") == 6
            and info.get("protocol_version") == 1 and type(info.get("model_execution")) is bool
            and (info["model_execution"] is False
                 or info.get("wake") is True and info.get("wake_providers") == ["codex"]),
            "v2 包版本、架构或协议不匹配。")
    local, glibc = platform.libc_ver()[1], info.get("minimum_glibc_symbols", "")
    require(isinstance(glibc, str) and re.fullmatch(r"\d+(?:\.\d+)+", glibc) and local
            and tuple(map(int, local.split("."))) >= tuple(map(int, glibc.split("."))), "glibc 低于安装包要求。")
    for name in ["codex-window-refresh-v2-bridge.so", "window-refreshd"]:
        common.verify_elf(output / name, arch)
    return info


def paths(plan):
    root = common.safe_path(plan["directory"] / "window-refresh-v2")
    service = plan["service"]
    require(service is not None, "v2 自动安装需要已核实的 CPA systemd 服务；容器按安装文档部署。")
    unit_dir = pathlib.Path(service["unit_dir"]) if service.get("scope") == "user" else pathlib.Path("/etc/systemd/system")
    suffix = hashlib.sha256(str(plan["config"]).encode()).hexdigest()[:12]
    name = f"window-refreshd-{suffix}.service"
    require(name != service["name"], "后台与 CPA 服务名称冲突。")
    runtime = root / "run"
    require(len(os.fsencode(runtime / "daemon.sock")) < 108, "CPA 目录太长，超过 Unix socket 路径上限。")
    return dict(root=root, runtime=runtime, data=root / "data", record=root / "installed.json",
                unit=common.safe_path(unit_dir / name), name=name,
                dropin=common.safe_path(unit_dir / (service["name"] + ".d") / "75-window-refresh-v2.conf"))


def render_unit(plan, locations, binary):
    service = plan["service"]
    user = "" if service.get("scope") == "user" else f"User={plan['user']}\nGroup={plan['gid']}\n"
    # Quote each systemd argument; safe_path has already rejected expansion and
    # control characters. Runtime/data directories are owned by the CPA user.
    content = ("[Unit]\nDescription=CPA Window Refresh v2 read-only backend\nAfter=network.target\n\n"
               "[Service]\nType=simple\n" + user +
               f'ExecStart="{binary}" --runtime-dir "{locations["runtime"]}" --data-dir "{locations["data"]}"\n'
               "Restart=on-failure\nRestartSec=3\nTimeoutStopSec=20\nUMask=0077\n"
               "NoNewPrivileges=true\n\n[Install]\nWantedBy=default.target\n")
    return content.encode()


def read_record(locations, plan):
    if not locations["record"].exists():
        return None
    record = json.loads(common.private_file(locations["record"]).read_text())
    require(record.get("backend") == "v2" and record.get("config") == str(plan["config"])
            and record.get("uid") == plan["uid"] and record.get("gid") == plan["gid"]
            and record.get("service") == plan["service"]["name"]
            and record.get("scope") == plan["service"].get("scope", "system"), "v2 安装记录与当前 CPA 不一致。")
    hashes = record.get("hashes")
    require(isinstance(hashes, dict) and hashes, "v2 安装记录缺少文件摘要。")
    for path, digest in hashes.items():
        path = common.safe_path(path)
        require(path in {locations["unit"], locations["dropin"]} or path.is_relative_to(locations["root"] / "releases")
                or path.parent == plan["plugin_dir"] and re.fullmatch(r"codex-window-refresh-v0\.2\.\d+-rc\.[1-9]\d*\.so", path.name),
                "v2 安装记录含非预期文件。")
        require(path.is_file() and hashlib.sha256(path.read_bytes()).hexdigest() == digest,
                f"已安装文件被修改或缺失，未覆盖：{path}")
    return record


def validate_cleanup_path(path, plan, locations):
    """A plugin cleanup can never include CPA configuration, credentials or peers."""
    path = common.safe_path(path)
    protected = [plan["directory"], plan["binary"], plan["config"], plan["plugin_dir"], locations["root"], locations["data"], locations["runtime"], locations["record"], locations["unit"], locations["dropin"]]
    require(not path.is_relative_to(locations["data"]) and not path.is_relative_to(locations["runtime"]), "清理路径落在当前 v2 数据目录，未删除。")
    home = pathlib.Path(pwd.getpwuid(plan["uid"]).pw_dir)
    auth = plan.get("data", {}).get("auth-dir", "~/.cli-proxy-api")
    if isinstance(auth, str) and auth:
        auth = pathlib.Path(str(home) + auth[1:] if auth.startswith("~/") else auth)
        if not auth.is_absolute():
            processes = common.detect_processes(plan["binary"])
            working = processes[0].get("cwd") if len(processes) == 1 else None
            working = working or plan.get("service", {}).get("WorkingDirectory")
            require(working and pathlib.Path(working).is_absolute(), "无法核实 CPA 相对账号目录，未清理。")
            auth = pathlib.Path(working) / auth
        protected.append(common.safe_path(auth))
        require(not path.is_relative_to(protected[-1]), "清理路径落在 CPA 账号目录内，未删除。")
    for name, config in plan["plugins"].get("configs", {}).items():
        if name != common.PLUGIN and isinstance(config, dict):
            for field in ["config_file", "state_dir"]:
                value = config.get(field)
                if isinstance(value, str) and pathlib.Path(value).is_absolute():
                    peer = common.safe_path(value)
                    require(not path.is_relative_to(peer), "清理路径与其他插件数据重叠，未删除。")
                    protected.append(peer)
    require(not any(p == path or p.is_relative_to(path) for p in protected), "清理路径覆盖 CPA、当前后台或其他插件，未删除。")
    return path


def cleanup_plan(plan, locations, previous, library, release_dir):
    candidates = []
    for name, fallback in [("policy", "window-refresh.yaml"), ("state", "window-refresh-state"),
                           ("env", "window-refresh.env"), ("record", "window-refresh-install")]:
        candidates.append(pathlib.Path(plan.get(name, plan["directory"] / fallback)))
    candidates.append(plan["directory"] / "window-refresh")
    # A rc.1 installation can still reference custom legacy locations. Save the
    # cleanup list until it finishes, so removing old wrapper fields loses no paths.
    candidates.extend(pathlib.Path(p) for p in (previous or {}).get("cleanup_pending", []))
    old_dropin = locations["dropin"].with_name("60-window-refresh.conf")
    candidates.append(old_dropin)
    candidates.append(old_dropin.with_name("99-window-refresh-quota-env.conf"))
    candidates.append(old_dropin.with_name("60-window-refresh.env"))
    for name in ["codex-window-refresh.so", "codex-window-refresh-v*.so"]:
        for path in plan["plugin_dir"].glob(name):
            if path != library and re.fullmatch(r"codex-window-refresh(?:-v\d+\.\d+\.\d+(?:-rc\.[1-9]\d*)?)?\.so", path.name):
                candidates.append(path)
    releases = locations["root"] / "releases"
    if releases.exists():
        candidates.extend(p for p in releases.iterdir() if p != release_dir and re.fullmatch(r"v0\.2\.\d+-rc\.[1-9]\d*", p.name))
    result = []
    for path in candidates:
        path = validate_cleanup_path(path, plan, locations)
        require(path not in {library, release_dir} and not library.is_relative_to(path) and not release_dir.is_relative_to(path), "清理目标包含当前版本，未删除。")
        if path.exists() and path not in result:
            require(path.is_file() or path.is_dir(), "清理目标不是普通文件或目录。")
            result.append(path)
    return result


def cleanup_deployment(targets, plan, locations):
    for target in targets:
        path = validate_cleanup_path(target, plan, locations)
        if path.is_dir():
            shutil.rmtree(path)
        elif path.exists():
            path.unlink()
    # Recovery copies exist only during activation. A successful deployment
    # deliberately retains neither old plugin data nor old executable backups.
    backups = common.safe_path(locations["root"] / "backups")
    if backups.exists():
        shutil.rmtree(backups)


def cpa_resource_url(plan):
    """Resolve the CPA listener, not a daemon address or an authenticated API."""
    data = plan.get("data", {})
    server = data.get("server", {})
    tls = server.get("tls", data.get("tls", {}))
    host = server.get("host", data.get("host", "")) or "127.0.0.1"
    host = {"0.0.0.0": "127.0.0.1", "::": "::1"}.get(host, host)
    port = server.get("port", data.get("port", 8317))
    authority = f"[{host}]:{port}" if ":" in host else f"{host}:{port}"
    scheme = "https" if isinstance(tls, dict) and tls.get("enable") else "http"
    base = plan.get("cpa_url") or f"{scheme}://{authority}"
    parsed = urllib.parse.urlsplit(base)
    require(parsed.scheme in {"http", "https"} and parsed.hostname and parsed.port != 0
            and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment
            and parsed.path in {"", "/"}, "--cpa-url 须为无认证信息和路径的 CPA HTTP(S) 地址。")
    return urllib.parse.urlunsplit((parsed.scheme, parsed.netloc,
            "/v0/resource/plugins/codex-window-refresh/healthz", "", ""))


def probe(runtime, version, host_pid=None, bridge_version=None, resource_url=None):
    """Run as the CPA user. Only read local IPC readiness; never query accounts."""
    class HTTP(http.client.HTTPConnection):
        def connect(self):
            self.sock = socket.socket(socket.AF_UNIX)
            self.sock.settimeout(2)
            self.sock.connect(str(runtime / "daemon.sock"))
    conn = HTTP("localhost", timeout=2)
    try:
        conn.request("GET", "/api/v2/overview")
        response = conn.getresponse()
        data = json.loads(response.read((2 << 20) + 1))
    finally:
        conn.close()
    require(response.status == 200 and data.get("version") == version[1:] and data.get("read_only") is True,
            "后台版本或健康状态不匹配。")
    request = json.dumps(dict(protocol_version=1, request_id="installer-readiness", method="bridge.hello")).encode()
    def read(sock, n):
        chunks = bytearray()
        while len(chunks) < n:
            chunk = sock.recv(n-len(chunks))
            require(chunk, "接入层响应不完整。")
            chunks.extend(chunk)
        return bytes(chunks)
    with socket.socket(socket.AF_UNIX) as sock:
        sock.settimeout(2)
        sock.connect(str(runtime / "bridge.sock"))
        sock.sendall(struct.pack("!I", len(request)) + request)
        size = struct.unpack("!I", read(sock, 4))[0]
        require(0 < size <= 65536, "接入层握手过大。")
        reply = json.loads(read(sock, size))
    hello = reply.get("result", {})
    activity = data.get("activity", {})
    require(reply.get("ok") is True and hello.get("version") == (bridge_version or version)[1:]
            and hello.get("protocol_version") == 1 and hello.get("events_supported") is True
            and activity.get("connected") is True and activity.get("bridge_instance_id") == hello.get("bridge_instance_id")
            and activity.get("host_process_id") == hello.get("host_process_id"), "后台与目标接入层尚未连通。")
    if host_pid is not None:
        parts = hello.get("host_process_id", "").split(":")
        require(len(parts) == 3 and parts[1] == str(host_pid), "接入层不属于当前 CPA 进程。")
    require(resource_url, "必须经 CPA 资源入口验证插件注册。")
    url = urllib.parse.urlsplit(resource_url)
    connection = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    conn = connection(url.hostname, url.port, timeout=2)
    try:
        conn.request("GET", url.path, headers={"Cache-Control": "no-cache"})
        response = conn.getresponse()
        raw = response.read(65537)
        require(response.status == 200 and len(raw) <= 65536,
                "CPA 未注册插件或资源入口不可用。")
        health = json.loads(raw)
    finally:
        conn.close()
    require(health.get("status") == "ready" and health.get("version") == version[1:]
            and health.get("read_only") is True and health.get("instance_id") == data["instance_id"],
            "CPA 资源入口未连接到目标后台实例。")
    return {"version": version, "instance_id": data["instance_id"], "bridge_instance_id": hello["bridge_instance_id"]}


def wait_ready(plan, locations, version, bridge_version, timeout=30):
    # A non-root CPA rejects a root socket peer. Use a small stdlib-only probe
    # under the CPA UID instead of copying credentials or weakening peer checks.
    code = ("import pathlib,sys\n" +
            "\n".join(["import http.client,json,socket,struct,urllib.parse", "class InstallError(Exception): pass",
                         "def require(ok, message):\n if not ok: raise InstallError(message)"]) + "\n")
    import inspect
    code += inspect.getsource(probe) + "\nprint(json.dumps(probe(pathlib.Path(sys.argv[1]),sys.argv[2],int(sys.argv[3]),sys.argv[4],sys.argv[5])))\n"
    resource_url = cpa_resource_url(plan)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        processes = common.detect_processes(plan["binary"])
        if len(processes) == 1 and processes[0]["uid"] == plan["uid"]:
            command = ["python3", "-c", code, str(locations["runtime"]), version, str(processes[0]["pid"]), bridge_version, resource_url]
            if plan["uid"] != 0:
                command = ["runuser", "-u", plan["user"], "--", *command]
            result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, timeout=6)
            if result.returncode == 0:
                return json.loads(result.stdout)
        time.sleep(.25)
    raise InstallError("未确认 CPA 插件注册、资源入口和目标后台连通；请检查 CPA 插件注册日志，不能报告更新成功。")


def install(args, plan, arch):
    locations = paths(plan)
    previous = read_record(locations, plan)
    if previous:
        print(f"当前后台版本：{previous['version']}；目标后台版本：{args.version}。")
    require(previous is not None or args.restart, "首次切换到 v2 必须显式 --restart，安全卸载 v1 并回收旧 Go 运行时。")
    filename = f"{PREFIX}_{args.version}_linux_{arch}.tar.gz"
    with tempfile.TemporaryDirectory(prefix="cwr-v2-package-") as temporary:
        temp = pathlib.Path(temporary)
        if args.package:
            archive, sums = pathlib.Path(args.package), pathlib.Path(args.checksums)
        else:
            archive, sums = temp / filename, temp / "SHA256SUMS"
            base = f"{common.REPOSITORY}/releases/download/{args.version}"
            common.download(base + "/" + filename, archive, 64 << 20)
            common.download(base + "/SHA256SUMS", sums, 65536)
        unpacked = temp / "unpacked"
        unpacked.mkdir()
        metadata = unpack(archive, sums, filename, unpacked, args.version, arch)
        bridge_version = metadata.get("bridge_version")
        require(isinstance(bridge_version, str) and re.fullmatch(r"v0\.2\.\d+-rc\.[1-9]\d*", bridge_version), "包缺少接入层版本。")
        print(f"目标接入层版本：{bridge_version}（与后台独立版本；CPA 插件列表可能显示此版本）。")
        bridge_bytes = (unpacked / "codex-window-refresh-v2-bridge.so").read_bytes()
        bridge_hash = hashlib.sha256(bridge_bytes).hexdigest()
        changed_bridge = previous is None or previous["bridge_sha256"] != bridge_hash
        require(not changed_bridge or args.restart, "接入层发生变化；请显式加 --restart，脚本不会自行重启 CPA。")
        if previous:
            require(common.version_key(args.version) >= common.version_key(previous["version"]), "不支持自动降级 v2 数据库；请按备份恢复。")
            require(plan["wrapper"].get("store", {}).get("version") == pathlib.Path(previous["library"]).name[len("codex-window-refresh-v"):-3], "CPA 插件选择已变更，未执行 v2 更新。")
        root = locations["root"]
        common.directory(root, 0o755, 0, 0)
        for name in ["runtime", "data"]:
            common.directory(locations[name], 0o700, plan["uid"], plan["gid"], strict=True)
        common.directory(root / "releases", 0o755, 0, 0)
        release_dir = root / "releases" / args.version
        common.directory(release_dir, 0o755, 0, 0)
        common.directory(plan["plugin_dir"], 0o755, 0, 0)
        for path in [locations["unit"].parent, locations["dropin"].parent]:
            if plan["service"].get("scope") == "user":
                common.user_service_directory(path, plan["uid"], plan["gid"])
            else:
                common.directory(path, 0o755, 0, 0)
        common.directory(root / "backups", 0o700, 0, 0, strict=True)
        backup = root / "backups" / (time.strftime("%Y%m%d-%H%M%S") + f"-{os.getpid()}-{time.time_ns()%1000000}")
        common.directory(backup, 0o700, 0, 0)
        transaction = common.Transaction(backup)
        daemon_service = dict(plan["service"], name=locations["name"])
        # Preserve the existing bridge filename on daemon-only updates, so a
        # config watcher cannot trigger a needless native lifecycle transition.
        library = pathlib.Path(previous["library"]) if previous and not changed_bridge else plan["plugin_dir"] / f"codex-window-refresh-{bridge_version}.so"
        cleanup = cleanup_plan(plan, locations, previous, library, release_dir)
        legacy_dropins = [locations["dropin"].with_name(name) for name in ["60-window-refresh.conf", "99-window-refresh-quota-env.conf"]]
        require(not any(p.exists() for p in legacy_dropins) or args.restart, "清除旧版服务环境需要显式 --restart；未切换或删除文件。")
        plugins = copy.deepcopy(plan["plugins"])
        plugins.update(enabled=True, dir=str(plan["plugin_dir"]))
        wrapper = plugins.setdefault("configs", {}).setdefault(common.PLUGIN, {})
        for field in ["config_file", "state_dir", "quota_transport", "management_url", "management_key_env"]:
            wrapper.pop(field, None)
        wrapper["enabled"] = True
        store = wrapper.setdefault("store", {})
        store.pop("local-reload-token", None)
        store.update(version=library.name[len("codex-window-refresh-v"):-3], **{"release-tag": "v" + library.name[len("codex-window-refresh-v"):-3]})
        text = common.replace_plugins(plan["text"], plugins)
        binary = release_dir / "window-refreshd"
        unit = render_unit(plan, locations, binary)
        dropin = f'[Service]\nEnvironment="CWR_V2_RUNTIME_DIR={locations["runtime"]}"\n'.encode()
        require(plan["config"].read_text() == plan["text"], "CPA 配置已被其他进程修改，未安装。")
        stopped_cpa, stopped_daemon = False, False
        try:
            if args.restart:
                common.service_run(plan["service"], "stop", plan["service"]["name"])
                stopped_cpa = True
            if previous:
                common.service_run(daemon_service, "stop", locations["name"])
                stopped_daemon = True
            database = locations["data"] / "backend.sqlite"
            if database.exists():
                require(stat.S_ISREG(database.lstat().st_mode) and database.stat().st_uid == plan["uid"], "数据库文件类型或拥有者不匹配。")
                with contextlib.closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as source, contextlib.closing(sqlite3.connect(backup / "backend.sqlite")) as dest:
                    source.backup(dest)
                os.chmod(backup / "backend.sqlite", 0o600)
            transaction.write(binary, (unpacked / "window-refreshd").read_bytes(), 0o755)
            if changed_bridge:
                # CPA can retain native mappings even after quiesce: the CPA
                # process is stopped before replacing any versioned library.
                transaction.write(library, bridge_bytes, 0o644)
            transaction.write(locations["unit"], unit, 0o644)
            transaction.write(locations["dropin"], dropin, 0o644)
            for legacy_dropin in legacy_dropins:
                if legacy_dropin.exists():
                    transaction.remove(legacy_dropin)
            if text != plan["text"]:
                transaction.write_watched(plan["config"], text.encode(), plan["text"].encode())
            common.service_run(daemon_service, "daemon-reload")
            common.service_run(daemon_service, "enable", locations["name"])
            common.service_run(daemon_service, "start", locations["name"])
            if stopped_cpa:
                common.service_run(plan["service"], "start", plan["service"]["name"])
            ready = wait_ready(plan, locations, args.version, bridge_version)
            hashes = {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in [binary, library, locations["unit"], locations["dropin"]]}
            record = dict(backend="v2", library=str(library), bridge_sha256=bridge_hash, bridge_version=bridge_version,
                          config=str(plan["config"]), uid=plan["uid"], gid=plan["gid"],
                          service=plan["service"]["name"], scope=plan["service"].get("scope", "system"),
                          cleanup_pending=[str(p) for p in cleanup], hashes=hashes, **ready)
            transaction.write(locations["record"], (json.dumps(record, indent=2) + "\n").encode(), 0o600)
        except BaseException:
            # Quiesce the new processes before restoring files/database. Never
            # start an old daemon against a schema advanced by a failed update.
            if stopped_cpa:
                common.service_run(plan["service"], "stop", plan["service"]["name"])
            common.service_run(daemon_service, "stop", locations["name"])
            transaction.rollback()
            if (backup / "backend.sqlite").exists():
                for suffix in ["-wal", "-shm"]:
                    sidecar = pathlib.Path(str(database) + suffix)
                    if sidecar.exists():
                        sidecar.unlink()
                common.atomic_write(database, (backup / "backend.sqlite").read_bytes(), 0o600, plan["uid"], plan["gid"])
            common.service_run(daemon_service, "daemon-reload")
            if stopped_daemon:
                common.service_run(daemon_service, "start", locations["name"])
            else:
                # Remove an enable symlink left by an unsuccessful first install.
                subprocess.run(common.service_command(daemon_service, "disable", locations["name"]), stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if stopped_cpa:
                common.service_run(plan["service"], "start", plan["service"]["name"])
            raise
        # Activation has committed. Cleanup failures must not attempt rollback
        # after any old data has been deleted; keep the pending list for retry.
        try:
            cleanup_deployment(cleanup, plan, locations)
            record["cleanup_pending"] = []
            record["cleanup_complete"] = True
            common.atomic_write(locations["record"], (json.dumps(record, indent=2) + "\n").encode(), 0o600)
        except (OSError, InstallError):
            raise InstallError("新版本已运行，但旧版清理未完成；未回退。请重试 v2 部署以继续清理。") from None
        print(f"v2 更新成功：{args.version}；已核对 CPA 注册资源入口、后台版本、接入实例和 CPA PID。")
        print(f"实际后台版本：{ready['version']}；实际接入层版本：{bridge_version}。请以后台版本判断 V2 是否更新。")
        print(f"CPA {'已按 --restart 重启' if stopped_cpa else '未重启'}；已清除 v1 数据、旧版本和临时备份，保留当前 v2 数据。")
        if metadata.get("prompt_generation") is True:
            print("额度查询本身不发送模型请求；短句候选与手动唤醒需明确确认。自动唤醒默认关闭，已有开启规则会保留并独立执行。")
        else:
            print("当前仅提供只读额度查询和活动观测；自动唤醒和模型生成在 v2 中不执行。")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["cpa-dir", "config", "plugin-dir", "working-dir", "user", "service", "package", "checksums", "cpa-url"]:
        parser.add_argument("--" + name)
    parser.add_argument("--version", default="latest")
    for name in ["restart", "user-service", "dry-run"]:
        parser.add_argument("--" + name, action="store_true")
    args = parser.parse_args()
    require(os.geteuid() == 0, "请以 root 运行 v2 安装脚本。")
    release = pathlib.Path("/etc/os-release").read_text()
    require(platform.system() == "Linux" and re.search(r'^ID=["\x27]?debian["\x27]?$', release, re.M)
            and re.search(r'^VERSION_ID=["\x27]?(12|13)["\x27]?$', release, re.M), "仅支持 Debian 12/13。")
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    require(arch, "仅支持 amd64/arm64。")
    require(args.version == "latest" or re.fullmatch(r"v0\.2\.\d+-rc\.[1-9]\d*", args.version), "只接受 latest 或 v0.2.X-rc.N。")
    require(bool(args.package) == bool(args.checksums) and (not args.package or args.version != "latest"), "离线安装须同时指定 --package、--checksums 和准确 --version。")
    require(not args.user_service or args.service, "--user-service 需要 --service。")
    # Reuse the v1 installer's verified CPA/service/path discovery only.
    for name, value in dict(model=None, defer_model=True, management_url=None, quota_transport="host", hot_reload=False, check_model=False).items():
        setattr(args, name, value)
    plan = common.infer(args)
    plan["cpa_url"] = args.cpa_url
    cpa_resource_url(plan)
    locations = paths(plan)
    common.verify_elf(plan["binary"], arch)
    print(f"V2 独立后台；CPA 服务：{plan['service']['name']}；运行用户：{plan['user']}")
    print(f"数据：{locations['data']}；运行目录：{locations['runtime']}")
    if args.dry_run:
        print("成功后清除 v1 策略、状态、环境文件、旧插件和后台版本、临时备份；保留当前 v2 数据。")
        print("仅预览；未下载、写入、清理或重启。首次切换、清理旧服务环境或接入层变化需要 --restart。")
        return
    if args.version == "latest":
        args.version = latest(arch)
    print(f"已选择 V2 后台版本：{args.version}（{arch}）。")
    lock = common.safe_path(plan["directory"] / ".window-refresh-install.lock")
    with os.fdopen(os.open(lock, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600), "r+") as stream:
        require(os.fstat(stream.fileno()).st_uid == 0, "安装锁必须由 root 拥有。")
        fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        install(args, plan, arch)


if __name__ == "__main__":
    try:
        main()
    except (InstallError, OSError, ValueError, KeyError, tarfile.TarError, subprocess.TimeoutExpired) as error:
        print(str(error) if isinstance(error, InstallError) else "v2 安装失败；请核对服务日志和备份。", file=sys.stderr)
        sys.exit(1)
