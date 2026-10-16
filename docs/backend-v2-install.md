# v2 只读预发布安装与更新

适用于 Debian 12/13、amd64/arm64、CPA ABI 1/schema 6。CPA 与后台使用相同 UID，通过私有 Unix socket 通信。v2 当前提供网页、额度查询、持久任务和活动观测；唤醒、短句生成及自动规则尚未实现。选择 v2 会停止 v1 功能，原策略与状态不会导入或删除。

## 首次切换

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install-v2.sh -o /tmp/cwr-v2-install.sh
sudo bash /tmp/cwr-v2-install.sh --restart
```

root 会话可省略 sudo。首次下载需要已有 curl。脚本安装缺少的运行依赖，识别当前 CPA 的配置、进程、用户及 systemd 服务；无法唯一确认时用参数明确指定，例如：

```sh
sudo bash /tmp/cwr-v2-install.sh --cpa-dir /opt/cpa --service cliproxyapi --restart
```

用户服务也支持，例如 CPA 为 root 的用户服务：

```sh
sudo bash /tmp/cwr-v2-install.sh --cpa-dir /root/cliproxyapi --service cliproxyapi --user-service --user root --restart
```

使用 `--dry-run` 预览，不下载安装包、不写入或重启。`--version v0.2.0-rc.1` 固定版本；默认只选择 v2 发布。首次切换及接入层二进制变化必须显式 `--restart`，脚本不会自动决定重启 CPA。首次重启同时回收旧版插件 Go 运行时映射。

## 后续更新

重新下载入口脚本并执行同一命令。若只改变后台与网页，省略 `--restart`，安装器只停止并启动独立后台；接入层改变则在写入前提示需要显式 `--restart`。两部分独立记录版本，后台新版本可以复用既有接入层。

普通 v1 的 `install.sh` 不自动选择 v2 包；检测到已有 v2 安装记录时拒绝覆盖。不能通过旧脚本把正在运行的 v2 改回 v1。

安装器完成以下核对后才报告成功：包的 SHA-256、ELF 架构、glibc 与协议；后台实际报告版本；接入层实际版本；后台消费的接入实例与当前 CPA 进程 PID。安装验证不会查询上游或发起模型请求。生产环境仍需用户通过 CPA 页面验证真实管理认证及账号额度访问。

网页仍从 CPA 原入口 `/v0/resource/plugins/codex-window-refresh/status` 打开，不额外开放网络端口。刷新网页后使用 CPA 管理密钥连接；密钥只保存在页面内存中。

## 文件与服务

在 CPA 目录下创建 `window-refresh-v2/`，包含版本化后台文件、`data/`、`run/`、安装记录及 `backups/`。`data/` 和 `run/` 由 CPA 用户拥有且权限为 0700；业务库权限 0600。备份和安装记录仅 root 可读。已有路径权限不符合要求时拒绝覆盖。

创建同一 systemd 管理器中的 `window-refreshd-<配置路径摘要>.service`，以及 CPA 服务的 `75-window-refresh-v2.conf` drop-in，后者只设置 `CWR_V2_RUNTIME_DIR`。其他插件、主配置非 plugins 字段、原 v1 环境文件、策略与状态保留。新后台使用自身独立数据目录。

先校验并下载完整包，再暂停需要切换的进程。更新前复制配置和安装文件；存在 SQLite 时通过在线备份接口创建一致副本。激活失败先停新后台，恢复文件与数据库，再启动旧服务。服务停止或恢复本身失败时停止操作并报告失败，不伪造更新成功。备份目录在安装输出及记录中提供。

## 离线安装与回退

下载对应架构包及同一发布的 `SHA256SUMS`，将包解压到临时目录，然后使用其中安装器：

```sh
python3 install-v2.py --cpa-dir /opt/cpa --service cliproxyapi \
  --version v0.2.0-rc.1 --package /path/codex-window-refresh-v2_v0.2.0-rc.1_linux_amd64.tar.gz \
  --checksums /path/SHA256SUMS --restart
```

以 root 运行。离线模式仍需预装依赖。安装器不接受自动降级数据库。需回退时先停止后台和 CPA，再根据对应 `backups/<本次安装>/index.json` 恢复原文件，移除本次新增文件，并恢复同一备份的 `backend.sqlite`（清除停机库旁的 `-wal/-shm`）；最后 daemon-reload 并启动所需旧服务。回到 v1 时恢复切换前 CPA 配置，停止并禁用新后台，移除新增的 CPA v2 drop-in，再重启 CPA；原 v1 业务状态没有被 v2 修改。

容器或非 systemd 部署目前不使用此自动安装器。按 [原型运行说明](backend-v2-prototype.md) 构建或提取两个产物，挂载相同私有 socket 目录和 UID，为 CPA 设置 `CWR_V2_RUNTIME_DIR`，由已有编排器管理独立后台；首次替换接入层需重启 CPA。当前没有自动容器迁移或 v1 数据导入。
