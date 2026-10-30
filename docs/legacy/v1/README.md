# V1 历史版本文档

本目录适用于 V1 Go 原生插件（`cmd/plugin`），版本见 `release/version.txt`。V2 使用独立后台和 SQLite，请从 [当前文档导航](../../README.md)进入。

| 文档 | 内容 |
| --- | --- |
| [安装与更新](install.md) | Debian 部署、热重载、Docker 与故障恢复 |
| [网页面板](panel.md) | V1 页面与规则保存 |
| [插件与管理接口](plugin.md) | V1 C ABI、管理 API、生命周期 |
| [配置契约](config-management.md) | V1 YAML/JSON 策略与拟议接口 |
| [运行时](runtime.md) | V1 调度顺序与限制 |
| [持久化](storage.md) | V1 原子文件存储与锁 |
| [CPA 适配器](cpa-adapter.md) | V1 宿主回调、额度解析与执行 |
| [固定宿主接口核对](cpa-integration.md) | V1 适配基线与验证范围 |
| [原始规则设计](codex-refresh-design.md) | 历史目标与验收场景 |
| [实施阶段](implementation-plan.md) | 历史规划及阶段性进度 |

`config.example.yaml` 和 `window-refresh check` 仅用于 V1；V2 的业务配置通过 API 保存到数据库，不读取这份策略文件。V1 的页面自动刷新默认关闭、文件持久化和串行网络执行描述不适用于 V2。
