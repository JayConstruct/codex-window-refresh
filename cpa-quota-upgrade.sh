#!/usr/bin/env bash
set -euo pipefail
script_directory=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
if [[ -f $script_directory/scripts/install.sh ]]; then
  exec bash "$script_directory/scripts/install.sh" "$@"
fi
temporary=$(mktemp -d /tmp/codex-window-refresh-bootstrap.XXXXXX)
trap 'rm -rf -- "$temporary"' EXIT
curl -fSL --retry 3 --connect-timeout 15 --max-time 180 \
  https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.sh \
  -o "$temporary/install.sh"
bash "$temporary/install.sh" "$@"
