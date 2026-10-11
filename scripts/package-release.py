#!/usr/bin/env python3
"""Package built Linux binaries and a disabled policy without local runtime data."""

import argparse
import gzip
import hashlib
import io
import json
import pathlib
import platform
import re
import struct
import subprocess
import tarfile


ROOT = pathlib.Path(__file__).resolve().parent.parent


def command(*args):
    return subprocess.check_output(args, cwd=ROOT, text=True).strip()


def elf_architecture(path):
    header = path.read_bytes()[:20]
    if header[:6] != b"\x7fELF\x02\x01":
        raise ValueError(f"{path.name} must be a 64-bit little-endian ELF binary")
    machine = struct.unpack_from("<H", header, 18)[0]
    if machine not in {62, 183}:
        raise ValueError(f"unsupported ELF machine: {machine}")
    return {62: "amd64", 183: "arm64"}[machine]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", default="dist")
    parser.add_argument("--commit", help="Source commit when packaging outside a Git checkout")
    parser.add_argument("--epoch", type=int, help="Source commit timestamp")
    args = parser.parse_args()
    if not re.fullmatch(r"v\d+\.\d+\.\d+-rc\.[1-9]\d*", args.version):
        parser.error("version must be a release candidate such as v0.1.0-rc.1")
    commit = args.commit or command("git", "rev-parse", "HEAD")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        parser.error("commit must be a full Git SHA")
    epoch = args.epoch if args.epoch is not None else int(command("git", "show", "-s", "--format=%ct", "HEAD"))
    if epoch < 0:
        parser.error("epoch must be nonnegative")
    library = ROOT / "bin/codex-window-refresh.so"
    cli = ROOT / "bin/window-refresh"
    arch = elf_architecture(library)
    if elf_architecture(cli) != arch:
        parser.error("plugin and configuration checker architectures differ")
    host_arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine())
    if arch != host_arch:
        parser.error("package on the native architecture after running the loading tests")
    symbols = command("readelf", "--version-info", str(library))
    versions = re.findall(r"GLIBC_(\d+)\.(\d+)", symbols)
    glibc = ".".join(map(str, max((int(a), int(b)) for a, b in versions))) if versions else None
    policy = (ROOT / "config.example.yaml").read_text()
    policy, count = re.subn(r"(?m)^enabled: true$", "enabled: false", policy, count=1)
    if count != 1:
        raise ValueError("cannot disable the default release policy")
    info = {
        "release": args.version,
        "commit": commit,
        "go_version": command("go", "version"),
        "platform": "linux",
        "architecture": arch,
        "minimum_glibc_symbols": glibc,
        "source_epoch": epoch,
        "host_abi": 1,
        "rpc_schema": 6,
        "real_account_validation": False,
    }
    installer, count = re.subn(r'(?m)^DEFAULT_VERSION = "[^"]+"$',
                              f'DEFAULT_VERSION = "{args.version}"',
                              (ROOT / "scripts/install.py").read_text(), count=1)
    if count != 1:
        raise ValueError("cannot set the bundled installer release version")
    members = {
        "codex-window-refresh.so": (library.read_bytes(), 0o644),
        "window-refresh": (cli.read_bytes(), 0o755),
        "config.example.yaml": (policy.encode(), 0o600),
        "INSTALL.md": ((ROOT / "docs/install.md").read_bytes(), 0o644),
        "PLUGIN.md": ((ROOT / "docs/plugin.md").read_bytes(), 0o644),
        "RUNTIME.md": ((ROOT / "docs/runtime.md").read_bytes(), 0o644),
        "NOTICE": ((ROOT / "cmd/plugin/NOTICE").read_bytes(), 0o644),
        "LICENSE.upstream": ((ROOT / "third_party/cliproxyapi-reference/LICENSE").read_bytes(), 0o644),
        "BUILD-INFO.json": ((json.dumps(info, ensure_ascii=False, indent=2) + "\n").encode(), 0o644),
        "install.py": (installer.encode(), 0o755),
    }
    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    name = f"codex-window-refresh_{args.version}_linux_{arch}"
    archive = output / (name + ".tar.gz")
    with archive.open("wb") as raw:
        with gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as tar:
                for filename, (data, mode) in sorted(members.items()):
                    entry = tarfile.TarInfo(name + "/" + filename)
                    entry.size = len(data)
                    entry.mode = mode
                    entry.mtime = epoch
                    entry.uid = entry.gid = 0
                    tar.addfile(entry, io.BytesIO(data))
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n")
    print(archive)


if __name__ == "__main__":
    main()
