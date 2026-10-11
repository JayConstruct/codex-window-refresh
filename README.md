# Codex Window Refresh

CLIProxyAPI 的 Codex 账号窗口唤醒插件，插件 ID 为 `codex-window-refresh`。

服务器安装见 [Debian 12/13 安装指南](docs/install.md)。测试版安装包从 [Releases](https://github.com/JayConstruct/codex-window-refresh/releases) 下载，按 `amd64` / `arm64` 选择并校验 SHA-256；包内示例策略默认关闭自动请求。

直接运行或 systemd 部署可使用 [自动安装脚本](scripts/install.py)：`sudo python3 install.py --cpa-dir /root/cliproxyapi`。支持预览、备份、升级与卸载；首次安装输入实际模型及管理密钥。先安装 `python3-yaml`，完整命令和服务参数见安装指南。

当前已实现 **Linux 原生插件原型**：后台协调器、持久化状态、额度查询桥、指定账号执行、活动观测及已认证管理 API。动态库已通过本地 C 假宿主加载验证；尚未在真实 CPA 宿主或生产账号上联调。

网页面板入口为 `/v0/resource/plugins/codex-window-refresh/status`，也通过 CPA 插件菜单注册。输入 CPA 管理密钥后可查看账号窗口与任务、调度容量和阻塞原因，编辑并保存规则、暂停账号、重新加载文件或重新检查调度。密钥仅保存在当前页面内存；查看页面不会主动查询上游额度或发送模型请求。详见 [面板使用说明](docs/panel.md)。

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

发布流程由 `.github/workflows/prerelease.yml` 管理。更新 `release/version.txt` 为新的 `vX.Y.Z-rc.N` 并添加对应 `docs/releases/` 说明后，主分支提交触发两种架构的测试与打包；修改工作流也会触发检查。全部通过才发布 Pre-release，已发布版本只做构建检查而不覆盖下载文件。需要手动重试时从 Actions 运行该工作流。
