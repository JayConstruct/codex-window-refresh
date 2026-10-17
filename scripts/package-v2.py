#!/usr/bin/env python3
"""Build a reproducible, separate v2 archive without v1 state or credentials."""
import argparse
import gzip
import hashlib
import importlib.util
import io
import json
import pathlib
import platform
import re
import subprocess
import tarfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("packaging", ROOT / "scripts/package-release.py")
packaging = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packaging)


def check():
    version = (ROOT / "release/v2-version.txt").read_text().strip()
    if not re.fullmatch(r"v0\.2\.\d+-rc\.[1-9]\d*", version):
        raise ValueError("invalid v2 release candidate")
    go = (ROOT / "internal/v2/version/version.go").read_text()
    native = (ROOT / "native/bridge/version.h").read_text()
    if f'const Version = "{version[1:]}"' not in go or not re.fullmatch(r'#define CWR_VERSION "0\.2\.\d+-rc\.[1-9]\d*"\s*', native):
        raise ValueError("invalid daemon or independently versioned bridge release")
    notes = (ROOT / f"docs/releases/{version}.md").read_text()
    if not notes.startswith(f"# {version} ") or len(notes.splitlines()) < 3:
        raise ValueError("missing v2 release notes")
    return version


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--commit")
    parser.add_argument("--epoch", type=int)
    parser.add_argument("--output", default="dist")
    args = parser.parse_args()
    version = check()
    if args.check:
        print(version)
        return
    commit = args.commit or packaging.command("git", "rev-parse", "HEAD")
    epoch = args.epoch if args.epoch is not None else int(packaging.command("git", "show", "-s", "--format=%ct", "HEAD"))
    if not re.fullmatch(r"[0-9a-f]{40}", commit) or epoch < 0:
        parser.error("a full source commit and nonnegative timestamp are required")
    bridge = ROOT / "bin/codex-window-refresh-v2-bridge.so"
    daemon = ROOT / "bin/window-refreshd"
    arch = packaging.elf_architecture(bridge)
    if packaging.elf_architecture(daemon) != arch or {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine()) != arch:
        parser.error("build and test both binaries on their native architecture")
    versions = re.findall(r"GLIBC_(\d+)\.(\d+)", "\n".join(packaging.command("readelf", "--version-info", str(p)) for p in [bridge, daemon]))
    glibc = ".".join(map(str, max((int(a), int(b)) for a, b in versions)))
    info = dict(backend="v2", release=version, commit=commit, source_epoch=epoch,
                bridge_version="v" + re.search(r'"([^"]+)"', (ROOT / "native/bridge/version.h").read_text())[1],
                platform="linux", architecture=arch, host_abi=1, rpc_schema=6,
                protocol_version=1, minimum_glibc_symbols=glibc,
                go_version=packaging.command("go", "version"), model_execution=True,
                wake=True, wake_providers=["codex"],
                prompt_generation=True, model_settings=True,
                prompt_generation_providers=["codex", "antigravity"],
                editable_generation_prompt=True,
                real_account_validation=False)
    members = {
        "codex-window-refresh-v2-bridge.so": (bridge.read_bytes(), 0o644),
        "window-refreshd": (daemon.read_bytes(), 0o755),
        "BUILD-INFO.json": ((json.dumps(info, indent=2) + "\n").encode(), 0o644),
    }
    for name, source in {"install-v2.py": "scripts/install-v2.py", "install.py": "scripts/install.py",
                         "INSTALL.md": "docs/backend-v2-install.md", "PROTOTYPE.md": "docs/backend-v2-prototype.md",
                         "NOTICE": "native/bridge/NOTICE", "LICENSE.upstream": "third_party/cliproxyapi-reference/LICENSE"}.items():
        members[name] = ((ROOT / source).read_bytes(), 0o644)
    output = pathlib.Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    name = f"codex-window-refresh-v2_{version}_linux_{arch}"
    archive = output / (name + ".tar.gz")
    with archive.open("wb") as raw:
        with gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=epoch) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as tar:
                for filename, (content, mode) in sorted(members.items()):
                    entry = tarfile.TarInfo(f"{name}/{filename}")
                    entry.size, entry.mode, entry.mtime = len(content), mode, epoch
                    tar.addfile(entry, io.BytesIO(content))
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_name(archive.name + ".sha256").write_text(f"{digest}  {archive.name}\n")
    print(archive)


if __name__ == "__main__":
    main()
