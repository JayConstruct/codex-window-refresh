# 文档导航

当前使用入口为 V2。后台/客户端版本以 `release/v2-version.txt` 为准，接入层独立版本见 `native/bridge/version.h`；部署要求以使用指南为准，历史方案和研究记录须结合其适用范围阅读。

## 使用指南（V2）

| 文档 | 内容 |
| --- | --- |
| [安装与更新](user/install.md) | 首次切换、后续更新、备份、回退、离线与服务配置 |
| [网页面板](user/panel.md) | 查询、模型与唤醒规则、任务历史、认证和自动更新 |
| [项目账号调度与重置卡](user/account-scheduling.md) | 客户端、项目映射、会话粘性和官方重置卡 |

## 开发资料

| 文档 | 内容 |
| --- | --- |
| [当前架构、构建与接口](development/architecture.md) | V2 已实现组件、API、资源预算与验证边界 |
| [检查与发布](development/testing.md) | V1/V2 构建、测试归属、文档链接校验和发布流程 |
| [调度台 UI 重构与设计稿](development/ui-redesign.md) | 页面结构、确认稿、真实数据口径及截图验证 |
| [账号开关](development/account-controls.md) | CPA 状态修改边界、显示偏好与唤醒诊断 |
| [账号模型目录](development/model-catalog.md) | 模型目录协议、身份校验与权限边界 |
| [共享核心规则](development/core-architecture.md) | `internal/core` 领域模型；V1 运行时描述仅适用于 V1 |
| [重建设计](development/design.md) | V2 设计目标，已被替代的迁移方案明确标记 |
| [实施契约](development/contracts.md) | 组件归属、私有协议、事务与验证要求 |

## 研究与历史分析

以下记录保留分析时的版本和验证条件，当前行为应查阅上面的使用指南与开发资料。

- [CPA API 优化实现（rc.20）](research/cpa-api-optimization.md)
- [CPA API 核对（2026-10-29）](research/cpa-api-review-2026-10-29.md)
- [面板设计参考](research/panel-design-references.md)
- [V1 前后端同步分析及修复状态](research/data-sync-analysis.md)
- [V1 额度查询故障分析](research/quota-query-review.md)
- [V1 查询性能优化](research/query-performance.md)

## V1 与发布记录

- [V1 文档索引](legacy/v1/README.md)：旧版安装、策略、API、运行时和存储。
- [发布说明](releases/README.md)：按 V1/V2 查看历史版本，已发布记录保留原版本语义。

## 文档维护

用户操作写入 `user/`，当前实现与开发契约写入 `development/`，带日期/版本的分析放入 `research/`，V1 专属资料放入 `legacy/v1/`。发布说明只记录对应版本的变化，当前安装指南不逐版重复历史介绍。

移动文档后同步更新引用与打包清单，运行 `make docs-check`。发布包保持安装器要求的文件名；打包器将包内已有文件的链接改为本地名称，其余链接固定到该包的源码提交，确保文件重命名后仍能打开对应资料。
