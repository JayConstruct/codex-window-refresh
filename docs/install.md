# Debian 12/13 测试版安装

本指南适用于支持原生插件 ABI 1、RPC schema 6 的 Linux CLIProxyAPI。已核对的官方提交为 `3de4e248f6bb12fc7dfafb183404f3b248578922`；安装前检查 CPA 实际版本和插件加载日志，旧版和其他 fork 的兼容性未验证。

安装包在 Debian 12 环境构建；CPU 架构、glibc 与 CPA 运行环境必须匹配。Docker 部署以容器内的架构和 libc 为准。Alpine/musl 镜像不能使用这些 glibc 安装包。

## 自动安装（直接运行 / systemd）

自动安装脚本支持 Debian 12/13 的直接运行与 systemd，Docker 使用下面的手动挂载流程。以 CPA 程序 `/root/cliproxyapi/cli-proxy-api` 为例：

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl python3 python3-yaml
curl -fL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.py -o /tmp/codex-window-refresh-install.py
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi --dry-run
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi
```

脚本从运行中的 CPA 进程检测用户、工作目录和 `-config` 参数；没有运行进程时须提供 `--user`。存在自定义主配置时使用 `--config /absolute/path/config.yaml`。已有相对插件目录但进程停止时，使用 `--working-dir` 指明 CPA 工作目录。不能唯一确定的路径会停止，不猜测。

systemd 部署应在预览与安装命令中均添加 `--service 实际服务名`。脚本核对服务程序与用户，并添加仅用于本插件的 `EnvironmentFile` override。首次交互安装输入实际可用模型及现有管理密钥，密钥输入不显示。安装完成询问是否重启；也可显式使用 `--restart`。直接运行模式保留原启动方式，提示设置环境变量后手动重启。

默认 `--version latest` 查询本仓库已发布的 Release，按版本号选择最新正式版或 rc 测试版（同一版本正式版高于 rc）；不使用排除测试版的 GitHub `/latest` 接口。先检查最新版本是否有当前架构安装包与校验和，再固定本次版本并下载、校验 SHA-256、检查 ELF、glibc 与策略。最新版本资产缺失或查询失败时明确停止，不悄悄退回旧版；GitHub API 限流或网络不可用时可用 `--version vX.Y.Z-rc.N` 指定版本并绕过版本查询。

`--dry-run` 不查询版本、不下载、不写入、不重启；默认只显示将在执行安装时解析最新版本。新策略默认关闭自动请求；安装会开启 CPA 主配置中的 `plugins.enabled`，因此其他已配置为启用的插件也会随宿主加载。其他插件配置保留，`plugins` 节的注释可能重新排版，原配置先备份。策略、窗口状态在升级时保留。

非交互安装需显式提供 `--yes --model 可用模型 --key-file /root/管理密钥文件`，密钥文件由 root 拥有且权限为 `0600`。不接受命令行中的原始密钥。生成的 `window-refresh.env` 同样由 root 拥有、权限 `0600`，按 systemd EnvironmentFile 语法保存，不能直接用 shell `source` 执行。新安装包也包含 `install.py`；首个 rc.1 包不含脚本，请用上述地址下载。

升级时重新下载脚本并重复执行同一安装命令，默认更新到最新已发布版本；也可用 `--version vX.Y.Z-rc.N` 或 `--version vX.Y.Z` 固定版本，不覆盖旧策略、状态。新安装包内附脚本也默认选择 latest。`--uninstall` 不查询版本，仅卸载本脚本安装的插件与本插件 override，保留策略、状态、密钥环境文件和备份；也需使用原来的服务与路径参数。备份位于 CPA 目录的 `window-refresh-install/`，权限 `0700`，每次操作的 `index.json` 记录原路径、权限和拥有者。文件写入失败会恢复已替换的文件；服务启动失败时保留备份和安装结果供排查，不声称已成功加载。

离线安装须同时传入固定版本，例如 `--version v0.1.0-rc.2 --package /path/package.tar.gz --checksums /path/SHA256SUMS`，不查询 GitHub。自动安装依赖 Python 3 与 `python3-yaml`，不依赖 Go 或 C 编译器。含 YAML 锚点、重复键或符号链接目标的配置需先整理或使用手动安装方式。

## 1. 下载与校验

执行 `uname -m`：`x86_64` 使用 `amd64`，`aarch64` 使用 `arm64`。以下为 x86_64 示例；ARM 服务器将文件名中的 `amd64` 换成 `arm64`。

```sh
curl -fLO https://github.com/JayConstruct/codex-window-refresh/releases/download/v0.1.0-rc.2/codex-window-refresh_v0.1.0-rc.2_linux_amd64.tar.gz
curl -fLO https://github.com/JayConstruct/codex-window-refresh/releases/download/v0.1.0-rc.2/SHA256SUMS
grep '  codex-window-refresh_v0.1.0-rc.2_linux_amd64.tar.gz$' SHA256SUMS | sha256sum -c -
tar -xzf codex-window-refresh_v0.1.0-rc.2_linux_amd64.tar.gz
cd codex-window-refresh_v0.1.0-rc.2_linux_amd64
```

包内 `BUILD-INFO.json` 记录源码提交、架构、工具链与实际依赖的 glibc 符号版本。首次安装无需服务器安装 Go 或 C 编译器。

## 2. 放置插件、策略与状态目录

以下以 CPA 工作目录 `/opt/cpa`、运行用户 `cpa` 为例；按实际安装位置和用户替换。将插件放入 CPA 已配置的插件目录，保留已有其他插件。

```sh
sudo install -d -m 0755 /opt/cpa/plugins
sudo install -m 0644 codex-window-refresh.so /opt/cpa/plugins/codex-window-refresh.so
sudo install -o cpa -g cpa -m 0600 config.example.yaml /opt/cpa/window-refresh.yaml
sudo install -d -o cpa -g cpa -m 0700 /opt/cpa/window-refresh-state
sudoedit /opt/cpa/window-refresh.yaml
```

示例策略默认 `enabled: false`，此时允许查询和观察，但不发起后台模型请求。将 `execution.model` 的占位符替换为该 CPA 实际支持的 Codex 模型名；按需修改时区、比例、时段和每日目标，再校验：

```sh
sudo -u cpa ./window-refresh check /opt/cpa/window-refresh.yaml
```

状态目录必须由 CPA 运行用户拥有，权限为 `0700`，不能多进程共用。首次复制策略；升级时保留现有策略和状态，替换 `.so` 后通过进程重启生效。

## 3. 配置 CPA

在 CPA 主配置现有 `plugins` 节点合并以下内容，不创建重复 YAML 键。`management_url` 的端口需与实际 CPA 一致，路径按已部署管理入口调整。

```yaml
plugins:
  enabled: true
  dir: /opt/cpa/plugins
  configs:
    codex-window-refresh:
      enabled: true
      priority: 0
      config_file: /opt/cpa/window-refresh.yaml
      state_dir: /opt/cpa/window-refresh-state
      management_url: http://127.0.0.1:8317/v0/management
      management_key_env: CPA_MANAGEMENT_KEY
```

管理桥需要 CPA 的现有管理密钥。将其设置到 **CPA 进程** 的 `CPA_MANAGEMENT_KEY` 环境变量，不写进策略文件或插件包装配置。

若用 systemd，创建仅管理员可读的 `/opt/cpa/window-refresh.env`，内容为 `CPA_MANAGEMENT_KEY=` 后填写现有管理密钥；在实际 CPA 服务的 override 中添加：

```ini
[Service]
EnvironmentFile=/opt/cpa/window-refresh.env
```

执行 `systemctl daemon-reload`，然后重启实际 CPA 服务。仅在登录 shell 中 `export` 不会传给已由 systemd 管理的进程。

## 4. Docker 部署

`.so` 必须匹配 CPA 容器架构。将插件、策略和状态目录挂载进容器，并设置容器环境变量，例如在已有 Compose 服务中合并：

```yaml
services:
  cliproxyapi:
    environment:
      CPA_MANAGEMENT_KEY: ${CPA_MANAGEMENT_KEY:?请设置现有管理密钥}
    volumes:
      - ./plugins:/CLIProxyAPI/plugins:ro
      - ./window-refresh.yaml:/CLIProxyAPI/window-refresh.yaml:ro
      - ./window-refresh-state:/CLIProxyAPI/window-refresh-state
```

创建宿主机状态目录并设为 `0700`，拥有者 UID/GID 与容器 CPA 进程一致。主配置中的插件路径对应改为 `/CLIProxyAPI/plugins`、`/CLIProxyAPI/window-refresh.yaml`、`/CLIProxyAPI/window-refresh-state`。管理桥的 `127.0.0.1` 指 CPA 所在容器；环境或挂载变更后重新创建容器。

## 5. 验证与开启规则

查看 CPA 插件加载日志。使用原有管理认证访问：

```text
GET /v0/management/codex-window-refresh/summary
GET /v0/management/codex-window-refresh/capabilities
GET /v0/management/codex-window-refresh/accounts
```

初期 `summary.enabled` 为 false，说明业务策略仍关闭；账号清单、窗口及额度查询应可观察。检查 `/capabilities` 的 `QuotaAdapterReady` 和 `/summary` 的 `last_error`。管理桥缺失密钥、地址错误或认证失败时，先修复再启用。

确认正常后将策略文件顶层 `enabled` 改为 true，通过管理认证调用 `POST /v0/management/codex-window-refresh/reload` 重新加载。建议先暂停不参与实测的账号，控制首次测试范围；`account_overrides.<稳定账号ID>.paused: true` 可暂停指定账号。

完整接口和当前边界见仓库 `docs/plugin.md`；安装包对应 `PLUGIN.md`。测试版尚未完成真实宿主和真实账号联调。零用量与未来 reset 不足以确认窗口未启动；任务可能等待正向证据。实际后台并发为 1。
