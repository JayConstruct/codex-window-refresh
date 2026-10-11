#!/bin/sh
set -eu
task_tmp=$(mktemp -d "${TMPDIR:-/tmp}/codex-window-refresh-smoke.XXXXXX")
trap 'rm -rf "$task_tmp"' EXIT HUP INT TERM
python3 - "$task_tmp" <<'PY'
import base64,json,pathlib,sys
root=pathlib.Path(sys.argv[1])
policy=root/'policy.yaml'
policy.write_text('enabled: false\n')
wrapper=f'enabled: true\npriority: 0\nconfig_file: {policy}\nstate_dir: {root / "state"}\n'
(root/'lifecycle.json').write_text(json.dumps({'schema_version':6,'config_yaml':base64.b64encode(wrapper.encode()).decode()}))
PY
cc -Wall -Wextra -Werror -o "$task_tmp/fake-host" cmd/plugin/testdata/fake_host.c -ldl
"$task_tmp/fake-host" "$(pwd)/bin/codex-window-refresh.so" "$task_tmp/lifecycle.json"
