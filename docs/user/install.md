# V2 安装、更新与回退

适用于 Debian 12/13、amd64/arm64、CPA 原生 ABI 1，插件协商 RPC schema 6 或 7；支持 schema 8 的 CPA 仍可协商为 7。CPA 与后台使用相同 UID，通过私有 Unix socket 通信。当前 **v0.2.0-rc.29 后台与 rc.28 接入层**提供“Codex 调度台”网页、额度查询、持久任务、普通用户请求及逐次上游尝试记录、分流分析、模型配置、短句候选生成、Codex 模型唤醒、项目账号调度、插件会话粘性和赠送重置卡管理。新安装的调度、重置卡查询及自动兑换默认关闭，已有明确保存的调度、查询和唤醒规则保留。选择 v2 会停止 v1 功能，部署成功后清除 v1 策略、状态、环境文件、旧版本与备份；后续更新保留 v2 数据。

**本次后台为 rc.29，原生接入层保持 rc.28，数据库保持 schema 12。** 从 rc.28 更新时，安装器核实接入层文件完全一致后可仅更新并重启后台；完成后强制刷新网页。从 rc.27 或更早版本升级须显式 `--restart`，同时更新两个服务端产物并重启 CPA，注册新增网页资源和流量管理路由。只替换后台不能补齐旧接入层的路由和业务事件采集。迁移保留已有账号、规则、任务及发送防重记录；新增业务记录从实际采集开始，不会重建旧版本未记录的上游尝试。客户端安装器同步标记 rc.29，启动器行为沿用 rc.27，不读取或改写 Codex 配置。详见[配置说明](account-scheduling.md)和 [rc.29 发布说明](../releases/v0.2.0-rc.29.md)。

## 首次切换

当前后台为 **v0.2.0-rc.29**，接入层为 **v0.2.0-rc.28**。从 rc.28 更新且接入层文件完全一致时，可以省略 `--restart`。首次切换及从 rc.27 或更早版本升级须显式 `--restart`，重启 CPA 加载配套接入层。数据库为 schema 12，保留现有 v2 数据和发送防重记录；须遵循下面的一致备份和降级限制，详见 [rc.29 发布说明](../releases/v0.2.0-rc.29.md)。

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install-v2.sh -o /tmp/cwr-v2-install.sh
sudo bash /tmp/cwr-v2-install.sh --version v0.2.0-rc.29 --restart
```

root 会话可省略 sudo。首次下载需要已有 curl。脚本安装缺少的运行依赖，识别当前 CPA 的配置、进程、用户及 systemd 服务；无法唯一确认时用参数明确指定，例如：

```sh
sudo bash /tmp/cwr-v2-install.sh --cpa-dir /opt/cpa --service cliproxyapi --restart
```

用户服务也支持，例如 CPA 为 root 的用户服务：

```sh
sudo bash /tmp/cwr-v2-install.sh --cpa-dir /root/cliproxyapi --service cliproxyapi --user-service --user root --restart
```

使用 `--dry-run` 预览，不下载安装包、不写入或重启。`--version v0.2.0-rc.29` 固定版本；默认只选择 v2 发布。首次切换、清理旧服务环境及接入层二进制变化必须显式 `--restart`，脚本不会自动决定重启 CPA。首次重启同时回收旧版插件 Go 运行时映射。

## 后续更新

重新下载 V2 入口脚本，默认更新最新已发布的 V2 测试版：

```sh
curl -fSL -H 'Cache-Control: no-cache' https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install-v2.sh -o /tmp/cwr-v2-install.sh && sudo bash /tmp/cwr-v2-install.sh --version latest --restart
```

从 rc.27 或更早版本更新到 rc.29 必须使用 `--restart`，同时加载新版接入层并迁移至 schema 12。客户端启动器本体未改变；已有 rc.27 客户端可继续使用，需要更新本地安装版本时可运行[客户端安装器](account-scheduling.md#项目与账号)。服务端更新后强制刷新网页，保留原部署所需的 `--cpa-dir`、用户服务等参数。若原命令固定了旧 `--version`，请替换为 `latest`，或指定本次 `v0.2.0-rc.29`。从 rc.28 更新时，安装器核实接入层文件与当前安装完全一致后，允许省略 `--restart`，仅重启后台。

`latest` 会按数值比较全部已发布 V2 rc 版本，不取发布列表第一条，也不使用排除预发布的 GitHub `/releases/latest`。下载的独立入口会重新获取两份 Python 安装器，不复用下载目录中的旧副本；只有完整源码目录（含上级 `go.mod`）使用本地安装器，运行前须自行更新源码。离线安装继续使用下文的包内 Python 入口。

**后台和接入层独立记录版本**。本次后台为 rc.29，接入层保持 rc.28。安装器会输出目标后台版本、目标接入层版本，成功后显示实际后台及接入层版本；也可在网页设置页的运行信息中查看后台版本和数据库版本。若安装器报告接入层变化、网络错误或验证失败，则尚未完成更新，不能仅凭下载成功判断。

普通 v1 的 `install.sh` 不自动选择 v2 包；检测到已有 v2 安装记录时拒绝覆盖。不能通过旧脚本把正在运行的 v2 改回 v1。

安装器完成以下核对后才报告成功：包的 SHA-256、ELF 架构、glibc 与协议；后台实际报告版本；接入层实际版本；后台消费的接入实例与当前 CPA 进程 PID；经 CPA 的公开资源入口返回的健康状态、版本与后台实例 ID。接入层 socket 已启动但 CPA 拒绝注册时，安装失败并回滚。安装验证不会查询上游或发起模型请求。生产环境仍需用户通过 CPA 页面验证真实管理认证及账号额度访问。

网页仍从 CPA 原入口 `/v0/resource/plugins/codex-window-refresh/status` 打开，不额外开放网络端口。刷新网页后使用 CPA 管理密钥连接；密钥只保存在页面内存中。

默认从 CPA 配置推断 HTTP(S) 监听地址。若须使用证书匹配的域名或其他可访问地址，可添加 `--cpa-url https://cpa.example.com:8317`；该地址不带路径或密钥，HTTPS 使用系统证书信任。健康检查仅返回后台就绪状态、版本和实例 ID，不访问账号或发送管理密钥。

## 备份与回退

升级前另存一致备份，包含任务、规则、模型配置、额度证据、已采集业务记录及发送防重记录。备份 SQLite 时应停止后台或使用 SQLite 在线备份；不能只复制正在使用的主数据库文件而遗漏 WAL。首次从 V1 切换成功后不会导入或保留 V1 业务数据，需要自行提前保存用于核对的副本。

数据库 schema 12 不能交给 rc.27 或更早的后台直接打开。回退须停止后台并恢复与目标后台匹配的一致数据库备份，不能只换旧二进制，也不能通过清空未知发送记录恢复可发送状态。

安装器在激活期间建立临时一致副本，失败时尝试恢复；成功后清理该副本。日后手动回退需要升级前另存的备份，不能依赖临时恢复文件。首次切换和后续更新的实际处理以本页为准；旧设计中的 V1 数据导入器未实现。

历史功能与 schema 变化见 [发布说明索引](../releases/README.md)，本页只描述当前安装与更新步骤。

## 文件与服务

在 CPA 目录下创建 `window-refresh-v2/`，包含当前后台文件、`data/`、`run/` 和安装记录。激活期间临时建立 `backups/`，成功后删除。`data/` 和 `run/` 由 CPA 用户拥有且权限为 0700；业务库权限 0600。临时恢复副本和安装记录仅 root 可读。已有路径权限不符合要求时拒绝覆盖。

创建同一 systemd 管理器中的 `window-refreshd-<配置路径摘要>.service`，以及 CPA 服务的 `75-window-refresh-v2.conf` drop-in，后者只设置 `CWR_V2_RUNTIME_DIR`。保留 CPA 主程序、主配置非 plugins 字段、账号目录与其他插件。清除本插件的 v1 策略、状态、环境文件、安装记录和备份，去掉包装中的旧字段与旧环境 drop-in；删除未选中的旧动态库和后台版本目录。当前选中的接入层依据安装记录保留；本次接入层为 rc.28，后台为 rc.29。

先校验并下载完整包，再暂停需要切换的进程。更新前复制配置和安装文件；存在 SQLite 时通过在线备份接口创建一致副本。激活失败先停新后台，恢复文件与数据库，再启动旧服务。服务停止或恢复本身失败时停止操作并报告失败，不伪造更新成功。激活未完成时恢复副本位于 `window-refresh-v2/backups/`；新版本验证成功后清理旧版和所有临时恢复副本。清理失败时明确报告新版本已运行、清理未完成，不在删除旧数据后尝试回退；重新部署会继续处理待清理路径。

## 离线安装与失败恢复

下载对应架构包及同一发布的 `SHA256SUMS`，将包解压到临时目录，然后使用其中安装器：

```sh
python3 install-v2.py --cpa-dir /opt/cpa --service cliproxyapi \
  --version v0.2.0-rc.29 --package /path/codex-window-refresh-v2_v0.2.0-rc.29_linux_amd64.tar.gz \
  --checksums /path/SHA256SUMS --restart
```

以 root 运行。离线模式仍需预装依赖。安装器不接受自动降级数据库。失败且未完成自动恢复时，先停止后台和 CPA，再按临时 `backups/<本次安装>/index.json` 恢复文件，并恢复同一副本的 `backend.sqlite`（清除停机库旁的 `-wal/-shm`），最后 daemon-reload 并启动旧服务。成功部署后不保留旧数据与回退副本，不能再依赖部署前的 v1 数据回退。

容器或非 systemd 部署目前不使用此自动安装器。按 [构建与运行说明](../development/architecture.md) 构建或提取两个产物，挂载相同私有 socket 目录和 UID，为 CPA 设置 `CWR_V2_RUNTIME_DIR`，由已有编排器管理独立后台；替换接入层后需重启 CPA，不能只重启后台。当前没有自动容器迁移或 v1 数据导入。
