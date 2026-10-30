"""Resolve repository Markdown links and adapt them to flat release archives."""

import pathlib
import posixpath
import re
from urllib.parse import quote, unquote, urlsplit, urlunsplit


INLINE_LINK = re.compile(r"(?P<prefix>!?\[[^\]\n]*\]\()(?P<target><[^>\n]+>|[^\s)]+)")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})")
SOURCE_URL = "https://github.com/JayConstruct/codex-window-refresh"


def rewrite_links(text, rewrite):
    """Visit inline Markdown destinations, leaving fenced examples untouched."""
    fence = None
    lines = []
    for number, line in enumerate(text.splitlines(keepends=True), 1):
        marker = FENCE.match(line)
        if marker:
            value = marker[1]
            if fence is None:
                fence = value
            elif value[0] == fence[0] and len(value) >= len(fence) and not line[marker.end():].strip():
                fence = None
            lines.append(line)
            continue
        if fence is not None:
            lines.append(line)
            continue

        def replace(match):
            target = match["target"]
            wrapped = target.startswith("<")
            updated = rewrite(target[1:-1] if wrapped else target, number)
            return match["prefix"] + (f"<{updated}>" if wrapped else updated)

        lines.append(INLINE_LINK.sub(replace, line))
    return "".join(lines)


def local_target(source, destination):
    parsed = urlsplit(destination)
    if parsed.scheme or parsed.netloc or not parsed.path:
        return None
    path = unquote(parsed.path)
    parent = "" if path.startswith("/") else str(pathlib.PurePosixPath(source).parent)
    target = posixpath.normpath(posixpath.join(parent, path.lstrip("/")))
    if target == ".." or target.startswith("../"):
        raise ValueError(f"link escapes repository: {destination}")
    return target


def render_document(root, source, members, commit):
    """Use archive filenames when present, otherwise immutable source links."""
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("documentation links require a full source commit")
    source = pathlib.PurePosixPath(source).as_posix()
    archived = {pathlib.PurePosixPath(path).as_posix(): name for name, path in members.items()}

    def rewrite(destination, number):
        target = local_target(source, destination)
        if target is None:
            return destination
        path = root / target
        if not path.exists():
            raise ValueError(f"{source}:{number}: missing documentation target: {destination}")
        parsed = urlsplit(destination)
        if target in archived:
            return urlunsplit(("", "", quote(archived[target], safe="/"), parsed.query, parsed.fragment))
        kind = "tree" if path.is_dir() else "blob"
        url = f"{SOURCE_URL}/{kind}/{commit}/{quote(target, safe='/')}"
        return url + (f"?{parsed.query}" if parsed.query else "") + (f"#{parsed.fragment}" if parsed.fragment else "")

    return rewrite_links((root / source).read_text(), rewrite).encode()
