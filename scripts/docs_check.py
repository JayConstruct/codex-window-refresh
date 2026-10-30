#!/usr/bin/env python3
"""Check relative file links in maintained repository Markdown documents."""

import argparse
import pathlib

from package_docs import local_target, rewrite_links


ROOT = pathlib.Path(__file__).resolve().parents[1]


def check_documents(root=ROOT):
    documents = sorted({root / "README.md", *root.glob("docs/**/*.md"), *root.glob("tests/**/*.md")})
    errors = []
    for document in documents:
        source = document.relative_to(root).as_posix()

        def check(destination, number):
            try:
                target = local_target(source, destination)
                if target is not None and not (root / target).exists():
                    errors.append(f"{source}:{number}: missing target: {destination}")
            except ValueError as error:
                errors.append(f"{source}:{number}: {error}")
            return destination

        rewrite_links(document.read_text(), check)
    if errors:
        raise ValueError("\n".join(errors))
    return len(documents)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=pathlib.Path, default=ROOT)
    args = parser.parse_args()
    try:
        count = check_documents(args.root.resolve())
    except (OSError, ValueError) as error:
        parser.exit(1, f"Documentation check failed:\n{error}\n")
    print(f"Documentation links checked: {count} files")


if __name__ == "__main__":
    main()
