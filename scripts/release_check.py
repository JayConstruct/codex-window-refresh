#!/usr/bin/env python3
"""Validate the release tag, embedded plugin version, and matching release notes."""

import argparse
import pathlib
import re


ROOT = pathlib.Path(__file__).resolve().parent.parent


def check_release(root=ROOT, expected=None):
    version = (root / "release/version.txt").read_text().strip()
    if not re.fullmatch(r"v\d+\.\d+\.\d+-rc\.[1-9]\d*", version):
        raise ValueError("release/version.txt must contain a release candidate tag")
    if expected is not None and version != expected:
        raise ValueError(f"requested version {expected!r} differs from {version!r}")
    source = (root / "internal/plugin/service.go").read_text()
    embedded = re.findall(r'^const Version = "([^"]+)"$', source, re.MULTILINE)
    if embedded != [version[1:]]:
        raise ValueError("plugin Version must match release/version.txt without the leading v")
    notes = (root / "docs/releases" / f"{version}.md").read_text().strip()
    lines = notes.splitlines()
    if not lines or not re.fullmatch(r"# " + re.escape(version) + r"(?:\s+.*)?", lines[0]):
        raise ValueError("release notes must start with a heading for the selected version")
    if not any(line.strip() and not line.lstrip().startswith("#") for line in lines[1:]):
        raise ValueError("release notes must describe the release")
    return version


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", help="Require this exact version, e.g. v0.1.0-rc.15")
    args = parser.parse_args()
    try:
        version = check_release(expected=args.version)
    except (OSError, ValueError) as error:
        parser.exit(1, f"Release check failed: {error}\n")
    print(version)


if __name__ == "__main__":
    main()
