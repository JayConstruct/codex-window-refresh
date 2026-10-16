# Debian 12/13 测试版安装

本指南适用于支持原生插件 ABI 1、RPC schema 6 的 Linux CLIProxyAPI。已核对的官方提交为 `3de4e248f6bb12fc7dfafb183404f3b248578922`；安装前检查 CPA 实际版本和插件加载日志，旧版和其他 fork 的兼容性未验证。

安装包在 Debian 12 环境构建；CPU 架构、glibc 与 CPA 运行环境必须匹配。Docker 部署以容器内的架构和 libc 为准。Alpine/musl 镜像不能使用这些 glibc 安装包。

## 自动安装（直接运行 / systemd）

### 一行全自动安装或更新

已有 CPA 且插件为 rc.21 或更新版的 Debian 12/13 服务器，直接执行以下命令更新，默认热重载：

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.sh -o /tmp/codex-window-refresh-install.sh && sudo bash /tmp/codex-window-refresh-install.sh
```

root 会话可省略 `sudo`。此命令要求已有 `curl`，缺失时先执行 `sudo apt-get update && sudo apt-get install -y ca-certificates curl`。完整下载成功后才执行脚本，不使用边下载边执行的管道。统一入口自动安装缺少的 `ca-certificates curl python3 python3-yaml`，默认使用 `--version latest --quota-transport host --defer-model --yes --hot-reload`；识别、校验、备份和文件回滚仍由同一个 Python 安装器负责，不复制安装逻辑。旧 `cpa-quota-upgrade.sh` 委托该入口。旧的独立检查和管理桥修复脚本已移除；模型诊断使用下文的 `--check-model`，可选管理桥通过安装器的 `--quota-transport management` 配置。

首次没有 `--model` 时跳过模型识别，不询问管理密钥，不猜测模型；策略为 `enabled: false`、`execution.model: ""`，自动查询默认关闭，手动查询始终允许。随后在面板填写并保存支持的模型，即可按账号确认手动唤醒；自动查询、自动唤醒需分别手动开启并保存。也可在一行命令末尾加 `--model 实际模型名`，首次写入该模型但仍不自动开启唤醒。已有策略、模型、窗口状态和旧密钥文件在更新时保留，`--defer-model` 不会关闭已开启的模型唤醒。直接使用 Python 安装器且不加此参数时，保留原有交互/模型识别行为。

默认热重载，不重启 CPA，也不需要管理 key。首次安装、rc.20 及更早版本或缺少本机版本文件时，显式追加 `--restart`，普通安装后重启已核实的服务一次；热重载失败不会自动转为重启。已是目标版本且插件文件一致时，不重复安装或重启。无法唯一识别路径、用户、服务或配置时停止并提示参数。非 systemd 直接运行且需要首次生效时使用 `--no-restart`，再按原方式手动重启；Docker 使用手动挂载流程。参数示例：

```sh
# 明确指定 root 的 CPA 用户服务
sudo bash /tmp/codex-window-refresh-install.sh --cpa-dir /root/cliproxyapi --user-service --service cliproxyapi.service --user root
# 首次安装或旧版首次启用本机版本文件（会重启核实的服务）
sudo bash /tmp/codex-window-refresh-install.sh --restart
# 预览默认热重载，或选择普通安装但不重启
sudo bash /tmp/codex-window-refresh-install.sh --dry-run
sudo bash /tmp/codex-window-refresh-install.sh --no-restart
```

`--dry-run` / `--check-model` 不安装系统依赖，依赖缺失时明确停止；远程下载的入口仍需下载 Python 安装器到临时目录，退出时清理，不下载 Release 或写入 CPA 文件。`--check-model` 非交互诊断需要 `--key-file` 或已有可复用的私有密钥文件。`--uninstall` 默认不重启，需要立即重启时显式加 `--restart`。源码目录可直接 `sudo bash scripts/install.sh`，此时使用同目录的 Python 安装器；其他参数透传，固定版本、离线安装与卸载规则不变。

### 交互安装与高级参数

自动安装脚本支持 Debian 12/13 的直接运行与 systemd，Docker 使用下面的手动挂载流程。以 CPA 程序 `/root/cliproxyapi/cli-proxy-api` 为例：

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl python3 python3-yaml
curl -fL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.py -o /tmp/codex-window-refresh-install.py
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi --dry-run
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi
```

脚本从运行中的 CPA 进程检测用户、工作目录和 `-config` 参数；同时从进程 cgroup 识别所属系统级或用户级 systemd 服务，只有唯一服务且其 `MainPID` 与 CPA 进程匹配、启动程序包含 CPA 二进制时才采用。用户服务还核对 cgroup 中的 UID 与进程用户，并通过该用户的会话总线查询，支持 root 的用户服务；不会把 `user@UID.service` 用户管理器当作 CPA 服务。容器、其他进程作为主进程的监督服务或无法核实的情况不自动采用。没有运行进程时须提供 `--user` 或 `--service`。存在自定义主配置时使用 `--config /absolute/path/config.yaml`。已有相对插件目录但进程停止时，使用 `--working-dir` 指明 CPA 工作目录。不能唯一确定的路径会停止，不猜测。

systemd 部署可以省略 `--service` 使用已核实的自动识别结果；未识别到时，在预览与安装命令中均添加 `--service 实际服务名`。脚本核对服务程序与用户。默认 `quota_transport: host`，无需向服务传入管理密钥；管理桥仅在 `--quota-transport management` 时启用 EnvironmentFile。首次自动识别模型可临时输入现有管理密钥，输入不显示，宿主模式不保存它；指定 `--model` 可完全跳过密钥输入。安装完成询问 `现在重启 CPA 服务 …？ [y/N]`，默认稍后重启；确认后执行服务重启并检查 active 状态。未确认服务的直接运行模式也询问是否现在手动重启，选择现在时给出另一终端操作提示，不代替用户停止或重建未知启动方式的进程。随后刷新 CPA 管理网页以显示插件菜单。

明确指定用户服务时添加 `--user-service`，并用 `--user` 指定所属用户（运行中的 CPA 可自动检测）。例如 root 的用户服务：

```sh
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi --user-service --service 实际服务名 --user root
```

用户服务的 daemon-reload、restart、is-active 均以所属用户执行 `systemctl --user`，通过 `/run/user/UID/bus` 连接；该用户的服务管理器和会话总线须已运行。override 写入用户管理器报告的 `UnitPath` 中、位于其主目录下的首个持久配置目录（通常 `~/.config/systemd/user/服务名.service.d/`），不写入系统服务目录。只有兼容管理桥模式下，非 root 用户服务需要用户拥有、权限 `0600` 的 `60-window-refresh.env`，放在该 override 目录内；root 的用户服务可读取原有 root 私有环境文件。用户目录缺失时仅在已有主目录下创建，拥有者保持为服务用户。原 CPA 目录中的 root 私有环境文件也保留，用于升级时复用管理密钥。卸载保留两处密钥文件，移除本插件的 override；写入或用户 daemon-reload 失败时回滚文件。用户主目录以外的自定义 UnitPath 不自动写入。

`--yes` 跳过交互询问，默认不重启；需要安装后自动重启时使用 `--yes --restart`，前提是指定了 `--service` 或已核实识别服务。`--dry-run` 与 `--check-model` 不询问重启。先前未关联服务的安装可在升级时关联核实的服务，保留策略及窗口状态；已关联服务不允许迁移到其他服务。服务 active 只确认进程运行，仍需检查插件加载日志和管理接口，不保证插件已成功注册。

省略 `--model` 时，脚本通过 CPA 管理接口读取已启用 Codex 账号及其各自的模型目录，选择所有这些账号共同注册的模型，排除 `codex-auto-review` 等名称含独立 `auto-review` 段的特殊审核模型。优先顺序为名称包含独立 `mini`、`nano`、`luna` 段的模型，其次为名称包含 `codex` 的模型，最后为其他共同模型；同一优先级按名称排序，不根据名称估算实际价格。暂时不可用但未禁用的账号也纳入交集，避免恢复后无法使用选中的模型。只执行目录 GET 查询，不发送生成请求；目录登记不保证上游请求一定成功。CPA 需已运行并允许本地管理认证。查询失败、没有合适的共同模型或账号超过 64 个时，交互安装提示手动填写；`--model 实际模型名` 可跳过自动识别。已有策略文件时保留原模型，升级不重新选择。若旧脚本已选择 `codex-auto-review`，使用下方只读诊断确认新选择，再在插件面板或策略文件中修改 `execution.model` 并保存、重新加载；重复安装不会覆盖已有策略。

管理密钥是登录 CPA 网页面板使用的原始密码，不是调用模型的 API Key，也不是 CPA 配置自动保存的 bcrypt 哈希。若查询失败，可先运行只读诊断，不下载安装包、不写入安装文件、不重启：

```sh
sudo python3 /tmp/codex-window-refresh-install.py --cpa-dir /root/cliproxyapi --check-model
```

脚本区分 HTTP 401（认证）、403（访问限制或封禁）、404（接口未启用或路径不兼容）、连接拒绝、超时、DNS、TLS 及非 JSON 响应，显示不含账号查询参数的接口地址。认证失败不自动重试。本机回环地址查询绕过系统代理，远程地址保留系统代理和 TLS 验证。可用 `--management-url http://127.0.0.1:实际端口/v0/management` 覆盖检测地址；v8 入口使用 `/v8/management` 或完整 `/v8/management/requests/api-call`，模型目录对应 `/credentials` 与 `/credentials/models`，仅管理桥模式下将桥配置为 v8 完整调用路径。systemd 部署诊断时也带原来的 `--service` 参数。

默认 `--version latest` 查询本仓库已发布的 Release，按版本号选择最新正式版或 rc 测试版（同一版本正式版高于 rc）；不使用排除测试版的 GitHub `/latest` 接口。先检查最新版本是否有当前架构安装包与校验和，再固定本次版本并下载、校验 SHA-256、检查 ELF、glibc 与策略。最新版本资产缺失或查询失败时明确停止，不悄悄退回旧版；GitHub API 限流或网络不可用时可用 `--version vX.Y.Z-rc.N` 指定版本并绕过版本查询。

`--dry-run` 不查询版本或模型、不下载、不写入、不重启；默认只显示将在执行安装时解析最新版本。新策略默认关闭自动请求；安装会开启 CPA 主配置中的 `plugins.enabled`，因此其他已配置为启用的插件也会随宿主加载。其他插件配置保留，`plugins` 节的注释可能重新排版，原配置先备份。策略、窗口状态在升级时保留。

默认宿主模式可使用 `--yes --model 实际模型名`，无需密钥文件。首次非交互自动识别模型或兼容管理桥模式需提供 `--yes --key-file /root/管理密钥文件`，密钥文件由 root 拥有且权限为 `0600`；可自动识别模型，也可添加 `--model 可用模型`。自动识别失败会在写入安装文件前停止，补充 `--model` 后重试。不接受命令行中的原始密钥。只有管理桥模式才生成 `window-refresh.env`，由 root 拥有、权限 `0600`；非 root 用户服务另存一份用户私有副本。两者都按 systemd EnvironmentFile 语法保存，不能直接用 shell `source` 执行。请使用上述地址下载最新脚本；已发布 rc.2 包内的旧脚本仍需手动填写模型。

从 rc.8 及更早版升级时，默认切换宿主查询，保留旧密钥文件并移除安装器自身 override 的环境文件依赖。若此前已明确选择管理桥，使用 `--quota-transport host` 可切换；原有业务规则和模型不变。

升级时重新下载脚本并重复执行同一安装命令，默认更新到最新已发布版本；也可用 `--version vX.Y.Z-rc.N` 或 `--version vX.Y.Z` 固定版本，不覆盖旧策略、状态。新安装包内附脚本也默认选择 latest。`--uninstall` 不查询版本，仅卸载本脚本安装的插件与本插件 override，保留策略、状态、密钥环境文件和备份；也需使用原来的服务与路径参数。备份位于 CPA 目录的 `window-refresh-install/`，权限 `0700`，每次操作的 `index.json` 记录原路径、权限和拥有者。文件写入失败会恢复已替换的文件；服务启动失败时保留备份和安装结果供排查，不声称已成功加载。

对于 `/root/cliproxyapi` 下、由 root 用户级 `cliproxyapi.service` 运行的 CPA，首次安装和更新可在 root 会话执行同一组命令（先安装上述依赖）：

```sh
curl -fL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.py -o /tmp/codex-window-refresh-install.py &&
python3 /tmp/codex-window-refresh-install.py \
  --cpa-dir /root/cliproxyapi \
  --user-service --service cliproxyapi.service --user root \
  --version latest --quota-transport host --restart
```

首次安装按提示识别或填写模型，更新保留原模型与策略；`--restart` 会在安装后立即重启 CPA，建议避开在途请求。需要无交互安装时使用本指南开头的统一入口，或给 Python 安装器增加 `--defer-model --yes`。旧升级脚本副本若仍含 `--version v0.1.0-rc.9`，请改用统一入口，否则仍会安装旧版本。

离线安装须同时传入固定版本，例如 `--version v0.1.0-rc.2 --package /path/package.tar.gz --checksums /path/SHA256SUMS`，不查询 GitHub。自动安装依赖 Python 3 与 `python3-yaml`，不依赖 Go 或 C 编译器。含 YAML 锚点、重复键或符号链接目标的配置需先整理或使用手动安装方式。

## 热重载升级

rc.21 起通过本机版本文件确认热重载，**无需管理 key、密钥文件或管理地址**。插件在现有状态目录自动生成 `runtime-version.json`，不需要用户创建或填写。

rc.20 及更早版本不会生成此文件。首次升级到支持版本文件的版本，先用一键命令加 `--restart` 完成普通安装并重启 CPA 一次；之后脚本默认热重载，升级到兼容新版时直接执行：

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.sh -o /tmp/codex-window-refresh-install.sh && sudo bash /tmp/codex-window-refresh-install.sh
```

沿用原来的路径、用户、服务及额度查询方式参数。已有管理桥配置不因热重载而改变；此模式不读取桥密钥，也不连接管理 HTTP 接口。下载公开 Release 和校验和仍需访问 GitHub，离线安装使用固定版本、安装包和校验和参数。

先用 `--hot-reload --dry-run` 查看路径；固定版本使用 `--version vX.Y.Z-rc.N`。一键脚本默认 `--hot-reload`，不自动加 `--restart`。显式 `--restart` 选择普通安装并重启；`--no-restart` 选择普通安装但不重启、不执行热重载确认。三种生效方式互斥。模型诊断和卸载不会继承默认热重载，卸载只有显式 `--restart` 才重启。直接调用 Python 安装器时，仍用 `--hot-reload` 显式选择热重载。

要求 CPA ABI 1 / RPC schema 6，并具有版本文件发现、`plugin.quiesce`、注册失败清理及 `plugin.reconfigure` 恢复机制（参考 CPA `3de4e248f6bb12fc7dfafb183404f3b248578922`）。安装器与 CPA 必须可见相同文件、PID 命名空间和 `/proc`；Docker 宜在容器内操作，CPA 主配置须可在原文件上读写，插件目录须支持原子写入版本化动态库。容器内仍须核对实际运行进程及路径。确认过程不依赖 v0/v8 管理路由、管理端口或 TLS 配置。

版本文件包含实际版本、阶段、实例 ID、CPA PID、进程启动时间、boot ID、实际加载库路径及配置确认标记；权限 `0600`，状态目录保持 CPA 用户拥有及 `0700`。不包含管理密钥、账号凭据、请求正文或用户活动。注册、重新配置、暂停、关闭及后台异常退出时更新，不进行持续高频磁盘写入。文件不是用户配置，不要手动修改。

安装器先核对本地安装记录、文件权限、路径及活着的 CPA 进程，再向主配置的 `store.local-reload-token` 写入随机操作标记，等待当前插件回写确认。确认旧版已读到固定版本后，放入独立的新版 `.so` 并切换选择；新版必须报告目标版本和新的实例 ID，再由同一实例回写第二次配置标记，才报告成功。标记只关联本机操作，不是管理 key。残留版本文件、同 PID 的重启进程、迟到旧实例或未读到新配置的插件都不能确认成功。

策略、状态目录、宿主查询方式和已有密钥环境保持不变。不会覆盖正在映射的旧库，也不因失败重启服务；新插件启动后继续原有自动规则。主配置的其他字段保留，`plugins` 节排版可能变化，原文件先备份。rc.25 起热重载写主配置时保留 inode，沿用 CPA 自身配置保存的写入方式，避免 Linux 文件监听留在被替换的旧文件上；写前校验和备份，写后 fsync，写入失败时尝试恢复原内容。其他安装文件继续原子写入。

新版注册失败时，CPA 先清理失败实例并恢复旧实例；安装器确认旧版运行且回写新标记后重新固定旧版。已尝试的版本文件保留，避免复用宿主仍保留的原生映射；同版本重试须普通安装并重启，或发布更高版本。如果无法确认接管或恢复，保留文件、当前版本选择和备份并报告状态未知，请检查 CPA 日志与 `runtime-version.json`；不要手动删库或清空业务状态。主配置被并发修改时停止，不覆盖外部更改。

旧版脚本（rc.24 及之前）的原子替换可能使 CPA 的文件监听丢失，此时版本文件可能保留旧版 `running`、空确认标记及升级前的更新时间。单靠这份旧文件不能证明当前进程状态；先核对安装记录、主配置与 CPA 进程。新版脚本修复后续写入，但不能在进程外重新注册已丢失的监听；无法在线恢复宿主监听时，需要显式重启 CPA 一次，再继续默认热重载。脚本不会自行重启或伪造确认。

Go `c-shared` 的旧映射通过 `RTLD_NODELETE` 保留到 CPA 进程退出，每次升级会增加驻留映射；频繁升级后可择机重启回收。切换会等待正在执行的插件回调，可能有短暂的观测交接窗口，不承诺所有前台回调零延迟或无遗漏；安装器不重启 CPA。已知前台占用和送达未知记录跨实例保留，真正进程重启清理旧占用。

仍可用普通安装加 `--restart` 更新版本化安装。CPA 可能在下一次进程启动时清理未选中的旧文件，安装记录允许历史文件已被清理；卸载核对并移除仍存在的安装器所属版本文件，保留业务策略、窗口状态和备份。

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

状态目录必须由 CPA 运行用户拥有，权限为 `0700`，不能多进程共用。首次复制策略；普通手动升级保留现有策略和状态，替换 `.so` 后通过进程重启生效。需要热重载时使用上文安装器流程；不要覆盖当前映射的库或只添加高版本文件却不固定选择。

## 3. 配置 CPA

在 CPA 主配置现有 `plugins` 节点合并以下内容，不创建重复 YAML 键。默认无需管理地址或密钥；`quota_transport` 可省略，默认 host。

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
      quota_transport: host
```

默认 host 模式使用宿主账号凭据，保留旧管理桥字段也不会要求密钥。使用兼容桥时添加以下三项：

```yaml
quota_transport: management
management_url: http://127.0.0.1:8317/v0/management
management_key_env: CPA_MANAGEMENT_KEY
```

只有管理桥需要 CPA 的现有管理密钥。将其设置到 **CPA 进程** 的 `CPA_MANAGEMENT_KEY` 环境变量，不写进策略文件或插件包装配置。

若用 systemd，创建仅管理员可读的 `/opt/cpa/window-refresh.env`，内容为 `CPA_MANAGEMENT_KEY=` 后填写现有管理密钥；在实际 CPA 服务的 override 中添加：

```ini
[Service]
EnvironmentFile=/opt/cpa/window-refresh.env
```

执行 `systemctl daemon-reload`，然后重启实际 CPA 服务。仅在登录 shell 中 `export` 不会传给已由 systemd 管理的进程。

## 4. Docker 部署

`.so` 必须匹配 CPA 容器架构。将插件、策略和状态目录挂载进容器，默认无需管理密钥环境变量，例如在已有 Compose 服务中合并：

```yaml
services:
  cliproxyapi:
    volumes:
      - ./plugins:/CLIProxyAPI/plugins:ro
      - ./window-refresh.yaml:/CLIProxyAPI/window-refresh.yaml:ro
      - ./window-refresh-state:/CLIProxyAPI/window-refresh-state
```

创建宿主机状态目录并设为 `0700`，拥有者 UID/GID 与容器 CPA 进程一致。主配置中的插件路径对应改为 `/CLIProxyAPI/plugins`、`/CLIProxyAPI/window-refresh.yaml`、`/CLIProxyAPI/window-refresh-state`。只有使用管理桥时才需向容器传入管理密钥；该模式的 `127.0.0.1` 指 CPA 所在容器。挂载或环境变更后重新创建容器。

## 5. 验证与开启规则

查看 CPA 插件加载日志。使用原有管理认证访问：

```text
GET /v0/management/codex-window-refresh/summary
GET /v0/management/codex-window-refresh/capabilities
GET /v0/management/codex-window-refresh/accounts
```

初期 `summary.enabled` 为 false，说明业务策略仍关闭；账号清单、窗口及额度查询应可观察。检查 `/capabilities` 的 `QuotaAdapterReady` 和 `/summary` 的 `last_error`。默认模式检查宿主凭据、账号代理与网络；只有管理桥模式需检查后台管理地址和密钥。

确认正常后将策略文件顶层 `enabled` 改为 true，通过管理认证调用 `POST /v0/management/codex-window-refresh/reload` 重新加载。建议先暂停不参与实测的账号，控制首次测试范围；`account_overrides.<稳定账号ID>.paused: true` 可暂停指定账号。

完整接口和当前边界见仓库 `docs/plugin.md`；安装包对应 `PLUGIN.md`。测试版尚未完成真实宿主和真实账号联调。零用量与未来 reset 不足以确认窗口未启动；任务可能等待正向证据。实际后台并发为 1。
