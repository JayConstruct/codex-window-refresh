# Codex 调度台 UI 重构

本次修改以用户确认的图片为视觉依据，保留 V2 原有任务、认证、草稿、版本冲突和未知发送防重规则。页面名称改为“Codex 调度台”；这只是显示名称，插件 ID、管理 API 前缀和安装入口保持原有契约。

## 页面结构

顶部导航依次为总览、账号、自动化、项目调度、运行记录、设置。自动化包含额度查询、窗口唤醒、重置卡、模型与提示词；运行记录包含用户请求、插件调用、分流分析、任务记录。窄屏保持导航横向滚动，数据表在自己的滚动容器内展示。

总览展示已核实可用窗口、等待重置、后台任务和需要关注的事项。容量分母为未手动禁用的账号；灰色分段表示其他未计入可用容量的账号，并不把所有账号都判定为额度未知。账号页的可用筛选与后端容量使用同一证据规则，排序在全池执行后分页。

项目规则与重置卡规则仍由同一设置事务保存，页面分开不会创建两个独立配置副本。保存结果未知或版本冲突时保留原草稿与只读核实入口。查看记录、筛选、翻页和页面自动更新不创建新的上游任务。

## 视觉依据

| 页面 | 设计图片 | 实现截图（本地示例数据） |
| --- | --- | --- |
| 总览 | [overview.png](../design/ui/overview.png) | [总览](../design/ui/preview/overview.png) |
| 账号 | [accounts.png](../design/ui/accounts.png) | [账号](../design/ui/preview/accounts.png) |
| 额度查询 | [automation-query.png](../design/ui/automation-query.png) | [额度查询](../design/ui/preview/query.png) |
| 窗口唤醒 | [automation-wake.png](../design/ui/automation-wake.png) | [窗口唤醒](../design/ui/preview/wake.png) |
| 重置卡 | [automation-cards.png](../design/ui/automation-cards.png) | [重置卡](../design/ui/preview/cards.png) |
| 模型与提示词 | [automation-models.png](../design/ui/automation-models.png) | [模型与提示词](../design/ui/preview/models.png) |
| 项目调度 | [projects.png](../design/ui/projects.png) | [项目调度](../design/ui/preview/scheduling.png) |
| 任务记录 | [tasks.png](../design/ui/tasks.png) | [任务记录](../design/ui/preview/records-tasks.png) |
| 设置 | [settings.png](../design/ui/settings.png) | [设置](../design/ui/preview/diagnostics.png) |
| 插件调用 | [plugin-calls.png](../design/ui/plugin-calls.png) | [插件调用](../design/ui/preview/records-plugin.png) |
| 用户请求 | [user-requests.png](../design/ui/user-requests.png) | [用户请求](../design/ui/preview/records-users.png) |
| 分流分析 | [routing-analysis.png](../design/ui/routing-analysis.png) | [分流分析](../design/ui/preview/records-analysis.png) |

图片中的示例数据仅用于设计与本地浏览器测试；生产页面读取真实后台数据，缺失数据保留为空、未知或未采集。不同账号名称、提示词、任务数量及实际视口会使内容自然变化。视觉对照基准为 1586 × 992 桌面视口，另检查手机、纯白及深色主题。

补充预览：[手机总览](../design/ui/preview/mobile-overview.png)、[手机用户请求](../design/ui/preview/mobile-history.png)、[纯白主题](../design/ui/preview/white-overview.png)、[深色主题](../design/ui/preview/dark-overview.png)。[截图检查结果](../design/ui/preview/review.json)保存各页视口、主题、页面溢出及浏览器错误检查记录。

## 数据与部署

新增业务请求及逐次执行观测使用 schema 12。升级前保存一致数据库备份；旧后台不能直接打开新库。原生接入层新增流量记录路由及页面资源，需要更新接入层并重启 CPA，不能只替换后台页面。

用户请求、上游尝试、已报告用量和换号恢复分别统计。分流原因来自当时记录；身份或因果关系缺失时不按时间猜测。调用方标识不是自然人计数。插件调用记录基于已保存的操作及响应证据，旧数据不能还原完整网络尝试。详细边界见 [架构说明](architecture.md) 与 [面板指南](../user/panel.md)。

## 验证

`make check`、`make panel-check` 和 `make v2-smoke` 覆盖后端、前端和原生假宿主链路。`CWR_DESIGN_REVIEW=1 node scripts/v2-panel-browser-check.mjs` 以本地假服务生成 12 页桌面、11 页手机及纯白/深色主题截图，写出 `review.json`，检查页面溢出、控制台错误及零写入；可用 `CWR_SCREENSHOT_DIR` 指定输出目录。正常浏览器检查还验证草稿、版本冲突、启停状态核实、模型请求幂等，以及同值轮询不替换 DOM 或丢失焦点。测试不会使用真实账号或调用真实上游模型。
