# Codex Window Refresh

CLIProxyAPI 的 Codex 账号额度查询与窗口唤醒插件，插件 ID 为 `codex-window-refresh`。当前主线为 **V2：C 原生接入层 + 独立 Go 后台 + SQLite**。

当前后台、客户端及原生接入层为 **v0.2.0-rc.28**，数据库为 **schema 12**。新版“Codex 调度台”采用横向导航，重构总览、账号、自动化、项目调度与设置，新增用户请求、插件调用和分流分析。设计稿及实现截图见 [UI 重构说明](docs/development/ui-redesign.md)，升级要求见 [发布说明](docs/releases/v0.2.0-rc.28.md)。

## 安装与更新

服务端支持 Debian 12/13、Linux amd64/arm64。首次切换 V2 或从 rc.27 及更早版本升级到 rc.28 时，执行：

```sh
curl -fSL https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install-v2.sh -o /tmp/cwr-v2-install.sh
sudo bash /tmp/cwr-v2-install.sh --version v0.2.0-rc.28 --restart
```

已有 V2 后续更新可改用 `--version latest`。安装器按数值选择最新已发布的 V2，包括 rc；本次接入层新增页面资源和运行记录路由，需要显式 `--restart` 加载。首次切换成功后会清除本插件的 V1 策略、状态、旧版本及临时恢复副本；后续更新保留 V2 数据。升级前应另存一致备份，schema 12 不能由旧后台直接打开，回退须恢复与旧后台匹配的数据库。

完整参数、用户服务、离线安装与失败恢复见 [V2 安装指南](docs/user/install.md)。下载与 SHA-256 校验文件见 [Releases](https://github.com/JayConstruct/codex-window-refresh/releases)。

客户端支持 Linux/macOS，需要 Python 3.10+ 和 curl，无需 sudo：

```sh
curl -fSL -H 'Cache-Control: no-cache' https://raw.githubusercontent.com/JayConstruct/codex-window-refresh/main/scripts/install-client.sh -o /tmp/cwr-client-install.sh
bash /tmp/cwr-client-install.sh
```

默认安装到 `~/.local/bin`。进入项目目录运行 `cwr-codex`，首次请求后在服务端选择项目账号并保存；提供商启动错误时按提示输入实际 ID，客户端会记住选择。详见 [项目账号调度与重置卡](docs/user/account-scheduling.md)。

V1 用户继续使用 [V1 安装指南](docs/legacy/v1/install.md)；`scripts/install.sh` 和 `cpa-quota-upgrade.sh` 仍是 V1 入口。

## 当前 V2 行为

- Codex 调度台统一展示账号容量、耗尽窗口、后台任务及待办；用户请求、插件调用与分流分析按真实观测展示，缺失证据保留为未知。
- 支持单账号、批量及可选自动额度查询；额度查询不发送模型请求。
- 页面连接后默认自动读取后台状态；“同步 CPA”只核对宿主本地账号，不查询上游额度。关闭页面不终止后台任务。
- 新安装的自动查询、自动唤醒、项目调度及重置卡查询/自动兑换默认关闭；升级保留用户已明确保存的规则。
- 手动唤醒先查询额度并核对账号、活动与发送保护，通过准入后才发送指定账号模型。模型完成和窗口启动分别核实，结果未知不自动重发。
- 短句候选支持指定 Codex 或反重力账号及实际可用模型；生成会消耗模型额度，应用和保存由用户明确操作。反重力不参与 Codex 窗口唤醒。
- 窗口时间依据上游证据；零使用率或未来 reset 时间不足以单独确认窗口未启动。

页面操作、规则与数据口径见 [V2 面板指南](docs/user/panel.md)。

## 文档与目录

[文档导航](docs/README.md)按使用指南、开发资料、研究记录、V1 历史资料和发布说明组织。

```text
cmd/window-refreshd/   V2 独立后台入口
native/bridge/         V2 C 原生接入层及假宿主测试
internal/v2/           V2 API、任务执行、规则、观测与 SQLite
internal/core/         共享领域规则
internal/cpa/          CPA 适配与额度解析，部分由 V2 复用
cmd/plugin/            V1 Go 原生插件入口
cmd/window-refresh/    V1 策略文件检查命令
internal/              其余 V1 配置、运行时、存储与面板
scripts/               安装、打包、检查与浏览器验证入口
scripts/tests/         Python 安装与打包工具测试
tests/web/             按 V1/V2 分开的网页 Node 测试
docs/                  分类文档与历史发布说明
release/               V1/V2 当前发布版本
third_party/           上游许可和固定源码来源清单
```

## 开发与验证

需要 Go 1.23+、CGO、C 编译器、Python 3 和 Node.js 18+。V2 原生构建还需要 pthread、libcurl 开发包和 json-c SONAME 5，详见 [架构与构建](docs/development/architecture.md)。

```sh
make check             # 文档链接、全部 Go 测试与 vet
make v2-build          # 独立后台和 C 接入层
make v2-smoke          # V2 网页测试及原生假宿主端到端验证
make panel-check       # V1 和 V2 网页 Node 测试
make release-check     # V1 和 V2 发布版本检查
```

测试范围、浏览器验证、V1 构建与双架构发布流程见 [开发检查与发布](docs/development/testing.md)。本地假宿主测试和空账号 CPA 注册验证不等于真实账号或生产负载验收，具体边界见 [架构与验证范围](docs/development/architecture.md)。
