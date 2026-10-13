# Codex Window Refresh

CLIProxyAPI 的 Codex 账号额度查询与窗口唤醒插件，支持手动查询、可选自动查询和按需模型唤醒。插件 ID 为 `codex-window-refresh`。

服务器安装见 [Debian 12/13 安装指南](docs/install.md)。测试版安装包从 [Releases](https://github.com/JayConstruct/codex-window-refresh/releases) 下载，按 `amd64` / `arm64` 选择并校验 SHA-256；包内示例策略默认关闭自动请求。

## 一键安装或更新

Debian 12/13 已有 CPA 时，首次安装和以后更新都执行同一行：

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install.sh -o /tmp/codex-window-refresh-install.sh && sudo bash /tmp/codex-window-refresh-install.sh
```

[统一入口](scripts/install.sh) 自动补齐依赖、识别 CPA 路径及系统级/用户级 systemd 服务、校验并安装最新 Release（含 rc 测试版）、备份和重启已核实的服务。升级保留已有策略、模型和窗口状态。首次无需密钥或模型输入，默认只查询额度；之后在面板填写实际支持的模型并开启模型唤醒，不猜测模型。CPA 目录或服务无法唯一识别时停止并提示参数，不重启未知进程；Docker 不适用。可在命令末尾追加 `--cpa-dir /实际路径`、`--model 实际模型`、`--no-restart` 或 `--dry-run`。重启会短暂中断在途请求。已下载源码可直接运行 `sudo bash scripts/install.sh`；旧 `cpa-quota-upgrade.sh` 也转入同一入口。

直接运行或 systemd 部署可使用 [自动安装脚本](scripts/install.py)：`sudo python3 install.py --cpa-dir /root/cliproxyapi`。默认安装最新已发布版本（包含 rc 测试版），支持预览、备份、升级与卸载；默认使用宿主凭据查询额度，无需后台管理密钥；首次自动识别模型时可临时输入网页登录密钥，或用 `--model` 跳过识别，无法识别时提示手动填写。可核实识别运行中的 CPA 系统级或用户级服务，交互安装结束询问是否重启；未知启动方式提示手动重启。用户服务可用 `--user-service --service 服务名 --user 用户名` 指定。可用 `--model` 指定模型、`--version` 固定版本。先安装 `python3-yaml`，完整命令和服务参数见安装指南。

当前已实现 **Linux 原生插件原型**：后台协调器、持久化状态、宿主额度查询与可选管理桥、指定账号执行、活动与正常请求额度观测及已认证管理 API。普通 HTTP/流式响应头和 WebSocket 额度事件可更新对应账号，完整有效的活跃窗口证据会推迟后台重复查询。手动查询始终允许；定期额度查询与模型唤醒默认关闭，需用户手动开启并保存，已有配置中显式开启的设置保持不变。“立即查询额度”分批查询额度接口，账号卡片的“查询此账号额度”只查询指定账号，两者都不发送模型请求。只查看额度时保持“启用模型唤醒”关闭，仍可手动查询并被动更新。面板按总览、账号额度、额度查询、模型唤醒和诊断与高级分区；页面自动刷新默认关闭，手动勾选后每 5 秒读取快照，也可随时点击“更新页面数据”。面板显示额度来源与手动查询进度，各页面共用草稿和保存栏。动态库已通过本地 C 假宿主加载验证；尚未在真实 CPA 宿主或生产账号上联调。

## v0.1.0-rc.11 查询、刷新与手动唤醒

- **手动查询始终允许**：批量查询与单账号查询不再需要开启权限开关；旧配置中的 `quota_query.manual: false` 不再阻止查询。仍需要管理认证、后台运行及额度接口就绪。
- **自动查询默认关闭**：需要在“额度查询”中手动开启“定期查询额度”并保存后生效。
- **页面自动刷新默认关闭**：需要在“总览”手动勾选“自动刷新页面（每 5 秒）”；无需保存，重新打开或断开后恢复关闭。也可随时点击“更新页面数据”读取快照。
- **自动模型唤醒默认关闭**：与额度查询独立，开启并保存后才会按自动规则发送模型请求；单账号手动唤醒需单独确认。手动额度查询本身不发送模型请求。
- **保留已有设置**：已有配置中显式开启的自动查询与模型唤醒不被升级强制关闭。
- **单账号手动唤醒**：账号卡片新增独立按钮，确认后使用已保存的模型向所选账号发送真实请求；无需开启自动唤醒，仍受账号可用性、前台活动与重复发送限制约束。
- **倒计时以服务端证据为准**：仅查询返回的完整五小时额度不会启动倒计时；只有实际请求后的活跃窗口证据才会显示服务端重置倒计时。

以上变化随 `v0.1.0-rc.11` 安装包生效。一键脚本从 GitHub Release 下载最新版本；完成升级并重启 CPA 后刷新管理网页。旧版本行为请以对应版本说明为准，更新 README 不会替换已安装的插件。

网页面板入口为 `/v0/resource/plugins/codex-window-refresh/status`，也通过 CPA 插件菜单注册。同源部署时优先复用 CPA 管理中心已保存的管理密钥并自动连接，刷新或重新打开无需重复输入；凭据缺失、无法读取或失效时提供手动连接。可查看账号窗口与任务、调度容量和阻塞原因，编辑并保存规则、暂停账号、重新加载文件或重新检查调度。模块只读取宿主存储，不保存手动输入的密钥；查看页面不会主动查询上游额度或发送模型请求。详见 [面板使用说明](docs/panel.md)。

设计规则：本地窗口到期后随机延迟，精确绑定对应账号；周重置优先且夜间不限人数；普通唤醒根据近期去重用户数补足账号容量；有效 5 小时窗口内不发送；错过时间后合并补发；发送后通过实际额度信息验证新窗口。

## 设计文件

- [用户规则与调度逻辑](docs/codex-refresh-design.md)
- [核心模型与并发边界](docs/core-architecture.md)
- [官方 CPA 接口核对](docs/cpa-integration.md)
- [配置与管理接口](docs/config-management.md)
- [实现阶段与验收边界](docs/implementation-plan.md)
- [运行时实现与限制](docs/runtime.md)
- [插件构建、配置与管理 API](docs/plugin.md)
- [额度与执行适配器](docs/cpa-adapter.md)
- [单账号额度查询故障检查与方案完善](docs/quota-query-review.md)
- [持久化存储](docs/storage.md)
- [配置草案](config.example.yaml)

窗口重置关系采用用户描述的账号行为，并通过实际观测核对。新窗口时间以服务端报告为准，不使用本地发送时间加 5 小时推算。

## 实现

`internal/core` 实现窗口证据、意图合并、容量计算和发送准入；`internal/runtime` 协调观测、规则与执行；`internal/storage` 在发送前原子落盘，并通过进程锁限制单实例；`internal/cpa` 对接官方宿主 RPC 和显式管理桥；`cmd/plugin` 导出 C ABI 入口。

管理摘要提供人数、容量缺口和阻塞原因，读取状态不等待网络请求。查询和生成有独立超时，失败查询退避且全轮查询数量受限；活动在途人数增量更新。额度解析拒绝矛盾的字段与倒计时。详见 [运行时说明](docs/runtime.md)。

已核对的 CPA 版本提供由认证身份派生的 `caller_scope`，按该字段在请求开始时去重，并通过生命周期结束事件释放在途记录。一个共享 key 的多人算一个身份，账号近期请求数也不等于独立用户数。首版只实现这一统计来源，其他身份模式明确拒绝应用，旧版兼容与完成请求统计降级尚未实现。

## 本地检查

需要 Go 1.23 或更新版本；依赖 `gopkg.in/yaml.v3`。构建 Linux 插件与加载测试还需 CGO、C 编译器，加载测试需要 Python 3：

```sh
make check
make race
make build
make native-smoke
make panel-check
```

`make plugin` 生成 `bin/codex-window-refresh.so`；`bin/window-refresh check /absolute/path/config.yaml` 验证策略文件。示例中的模型占位符必须替换成部署宿主实际可用的模型名；初次联调建议将 `enabled` 设为 `false`。宿主配置见 [插件说明](docs/plugin.md)。

`make panel-check` 需要 Node.js 18 或更新版本，验证面板的认证、草稿、保存和请求行为。

首版实际生成并发为 1。没有上游窗口状态的正向证据时不会唤醒；零使用率和未来 reset 时间本身不足以确认窗口未启动。官方宿主目前也没有前台请求与后台唤醒之间的原子互斥接口，插件只能查询并观察后尽量避开。分布式执行尚未实现。

发布流程由 `.github/workflows/prerelease.yml` 管理。更新 `release/version.txt` 为新的 `vX.Y.Z-rc.N` 并添加对应 `docs/releases/` 说明后，主分支提交触发两种架构的测试与打包；修改工作流、脚本或脚本测试也会触发检查。全部通过才发布 Pre-release，已发布版本只做构建检查而不覆盖下载文件。需要手动重试时从 Actions 运行该工作流。
