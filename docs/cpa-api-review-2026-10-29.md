# CPA 最新 API 与项目优化分析（2026-10-29）

核对范围：CLIProxyAPI 官方最新正式版 **v8.0.47**，发布时间为 2026-10-29 00:27 UTC（北京时间 08:27），tag 指向 `90c447d3a8727d115aa7552ec93ce023736ed492`；本项目当前提交为 `a778225`，V2 后台与原生桥均为 `0.2.0-rc.19`。以下上游源码链接固定到该正式版提交。

结论：继续沿用“轻量原生桥 + 独立后台”的架构。优先修正 V2 查询的全局代理继承，接入新增的按账号模型目录回调，再减少账号同步和响应观察的开销。当前 rc.19 已实现 schema 7 的请求正文省略；无需为使用 schema 8 而接收或改写正文。

本次只新增分析文档；验证使用本地假宿主和模拟响应，未读取真实认证文件、查询真实额度或发送真实模型请求。

## 已核实的上游变化

| API / 能力 | 最新契约 | 对本项目的影响 |
| --- | --- | --- |
| 原生 ABI / RPC schema | ABI 仍为 1；最新 schema 为 8；宿主接受插件声明较低 schema | rc.19 在宿主提供 8 时返回 7，符合当前协商校验，不构成必然不兼容 |
| schema 7 请求正文省略 | `request_interceptor_omit_body_before/after` | rc.19 已实现，属于已有优化，应先确认生产 CPA 和桥接层均已更新 |
| schema 8 正文局部修改 | `request.patch_body_before/after`，接收原始字节并返回局部 edits | 用于需要修改请求正文的插件；本项目只消费可信元数据，继续省略正文更合适 |
| 按账号读取模型 | 新增 `host.auth.models`，请求可带 `auth_index`，返回账号 ID、索引、provider、模型 ID 列表 | 可以替代浏览器端多次管理 API 读取，并增加后台发送前检查 |
| v8 模型目录刷新 | `POST /v8/management/models/refresh`，也有 `/routing/model-definitions/refresh` | 可增加显式“刷新 CPA 模型目录”；需区分远程目录刷新与本地注册模型读取 |
| 管理接口兼容 | `/v0/management` 继续支持；插件扩展保留各自声明的 v0 路径 | 无需批量替换现有面板路由 |
| 凭据列表 | 支持 `name`、`auth_index` 过滤与 `page`、`page_size` 分页 | 单账号启停核实可以减少全量列表传输；分页本身不保证原子快照 |
| 额度 provider | v8 凭据列表可返回 `quota_plugin_id`，插件额度有统一管理入口 | 可选接入 CPA 的统一额度展示；不能据此假定所有 Codex 账号都具备内置额度查询 |

依据：[正式版发布说明](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.47)、[ABI/schema 定义](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/sdk/pluginabi/types.go#L5)、[协商实现](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/rpc_client.go#L102)、[模型回调](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/auth_callbacks.go#L137)、[v8 管理路由](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/api/server_management_v8.go)、[v0 兼容说明](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/docs/management-api-v8.md#L228)。

## 优先级与实施建议

### P0：修正 V2 额度查询未继承 CPA 全局代理的问题

**源码事实：** [V2 查询 transport](../internal/v2/executor/source.go) 的 `client()` 在凭据未提供代理时将代理设为 `direct`，同时将 `http.Transport.Proxy` 置空。因此，只在 CPA 全局配置了代理的部署，其模型请求和插件额度查询可能走不同出口。这里尚未进行生产网络复现，结论来自明确的代码路径。

V1 的 [NativeReader](../internal/cpa/native_quota.go) 已有“无账号代理走宿主 HTTP，有账号代理走专用 transport”的实现，可作为适配参考。最新 CPA 的 `host.http.do` 仍通过宿主 HTTP client 继承全局配置，但它的请求结构没有 AuthID，也不自动提供指定账号 token 或账号代理。[上游 HTTP 执行](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/host_callbacks.go#L200)、[代理选择](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/http_bridge.go#L217)。

建议明确区分：账号显式 direct、账号专用代理、未指定账号代理。第三种通过受限桥接操作使用宿主 HTTP；另外两种保留现有连接池。桥接需支持操作取消、固定额度 URL、方法和 header 白名单，不能简单开放任意 HTTP 转发；host HTTP 回调也不能当成精确账号认证接口。

验收：仅全局代理、仅账号代理、显式 direct、代理认证、超时取消，以及查询前后账号替换。所有路径继续保留绑定校验，不通过模型请求刷新 token。

### P1：接入 `host.auth.models`，统一账号模型目录

**源码事实：** [面板 `reloadAccountModels()`](../internal/v2/daemon/web/panel.js) 目前执行“全量 `/auth-files` → `/auth-files/models?name=...` → 再次全量 `/auth-files`”。V2 的 [Go 回调白名单](../internal/v2/hostlink/client.go) 和 [原生桥白名单](../native/bridge/bridge.c) 都尚未允许 `host.auth.models`。

建议增加受限回调、后台账号模型读取入口及短期缓存，绑定 `bridge_instance + account_id + auth_index`，账号绑定变化时失效。浏览器通过现有插件认证入口读取；发送前由后台核对所选账号注册模型。优先按单账号查询，避免一次取回所有账号模型超过私有 IPC 的 4 MiB 帧限制。

这是 10 月 20 日新增的回调，不能仅根据 ABI 1 或 schema 7 推断旧宿主支持。需要单独记录支持状态，并保留旧管理读取路径或明确显示能力不可用。模型“已注册”也不保证此刻上游有权限或额度，不以实际试发作为目录检查。[新增提交](https://github.com/router-for-me/CLIProxyAPI/commit/4846a2893bd51f4057f3272866b1176b2f804cc4)、[返回结构](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/sdk/pluginapi/types.go#L905)。

### P1：减少 30 秒全量身份核对与数据库写入

**源码事实：** [inventoryLoop / syncInventory](../internal/v2/ingest/ingest.go) 每 30 秒列举账号，再对每个可核对账号调用 `Identity()`；后者通常各执行一次 `host.auth.get_runtime` 和 `host.auth.get`。对于 N 个正常 Codex 账号，单轮约有 `1 + 2N` 次宿主回调，且每次 `host.auth.get` 都读取物理凭据 JSON。1000 账号约为 2001 次/轮；这是代码计数推导，不是性能测量。

建议分层同步：常规状态同步复用 `host.auth.list` 的结果；将完整身份对账分批执行，维护有时效的绑定证据；查询及模型发送继续进行严格的即时绑定核对。注意只比较 ID、mtime 或 UpdatedAt 不能证明同名账号未替换，OAuth 刷新也不能直接当成新账号。缓存过期时必须使对应被动额度证据失效，不能放宽发送条件。

[SyncInventory](../internal/v2/store/inventory.go) 对未变化账号也更新 `updated_at` 和绑定的 `confirmed_at`。可按实际变化条件写入，合并确认时间刷新，分别记录同步心跳与业务数据变化。现有 `accounts_update` trigger 已按字段变化限制 revision 增长，不能把它描述为“每轮必然使所有账号 revision 增长”。

验收：100/1000 账号下的同步耗时、回调数、凭据读取数、WAL 写入量；同名替换、正常 token 刷新、慢账号及后台重启。需同时检查桥接绑定有效期与观测恢复逻辑。

### P1：优化响应观察的正文传输与逐 chunk 调用

**源码事实：** [原生桥注册](../native/bridge/bridge.c) 仍启用 `response_interceptor`、`response_stream_interceptor` 和 WebSocket observer；[事件观察器](../native/bridge/events.c) 对普通 SSE 只使用 `ChunkIndex == -1` 的额度响应头，其他 chunk 在扫描外层 JSON 后忽略。

最新 CPA 仍对 stream interceptor 的每个 chunk 执行 RPC。schema 3/5 已在正文 chunk 省略请求正文及历史块，但 header-init 仍可携带 `OriginalRequest`、`RequestBody`，非流式响应观察也会复制请求/响应正文。schema 7 的请求拦截省略标记不覆盖这些响应路径。[宿主响应拦截实现](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/adapters_interceptors.go#L329)。

项目内可先减少无关 chunk 的字段解析和分配，并对长 prompt 的首帧单独测量。较大收益需要宿主提供仅响应头观察、仅 stream 初始化回调或省略响应侧请求正文的能力；这些不能当作当前已存在的 API。也可评估 `usage.handle` 的 ResponseHeaders 能否覆盖所需路径，经覆盖验证后才考虑替换部分观察钩子。

不能直接删除 stream/WebSocket 观察：这会降低被动额度覆盖率，并增加主动额度查询。也不能把延迟消费重复响应头当作新额度观测。验收应测长上下文 HTTP、SSE、WebSocket 的宿主 RSS、分配量、每 chunk CPU、首 token 延迟及被动额度覆盖率。

### P2：利用凭据过滤接口，减少面板的全量读取

**源码事实：** [refreshConfirmedStatuses()](../internal/v2/daemon/web/panel.js) 在存在本页已确认启停记录时，每轮仍拉取全量 `/auth-files`；面板普通轮询间隔约为 5 秒，有活动查询任务时约为 1.5 秒，后台标签页约为 30 秒。

最新列表接口支持 `name` 与 `auth_index` 过滤，以及分页。单账号核实可先改用过滤读取；若已确认账号很多，再考虑合并读取或限制刷新频率，避免以 N 次小请求替代一次全量请求。返回后仍校验 ID、文件名、索引和唯一性。过滤是读取优化，不是账号替换的原子保护。[上游实现](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/api/handlers/management/auth_files.go#L106)。

模型目录回调接入后，浏览器的两次全量目录核对也可同步减少。现有后台已提供 revision、分页和局部 DOM 更新，适合在此基础上按页面需求降低读取频率；当前原生管理桥为有限响应转发，改成 SSE 推送会增加协议和连接生命周期工作，优先级较低。

### P2：补齐限流与能力诊断

最新 CPA 已区分 `turn_rate_limit_exceeded` 这类临时容量限制，并支持插件执行错误的 `retry_after_seconds` 提示。这里的重试提示针对相应执行错误契约，不能假定它自动出现在所有 host model stream 响应中。[临时容量提交](https://github.com/router-for-me/CLIProxyAPI/commit/9a35fa12b2f388b235afec48736b7dfecfcc4de5)、[错误结构](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/sdk/pluginabi/types.go#L139)。

项目可为只读额度查询保留并校验 HTTP `Retry-After`，形成查询退避依据；将“短时请求限制”“上游额度耗尽”“CPA 本地冷却”分开显示。模型请求送达不明时仍保留 unknown 与防重记录，不能依据 429 或 retry hint 自动重发唤醒。

诊断页可增加实际协商 schema、请求正文省略状态、按账号模型回调支持状态、查询代理模式、同步耗时及每轮回调数。当前桥接 hello 尚未暴露全部这些信息。能力探测只执行必要的本地读取，不用模型调用推断能力。

### P3：可选提供标准 QuotaProvider

上游现在可将 provider 对应的 `quota_plugin_id` 提供给 v8 管理面板。若希望 CPA 主面板直接显示本插件已观测的 Codex 额度，可评估注册 QuotaProvider，将已归一化且仍有效的缓存结果映射到官方 `QuotaFetchResponse`。

该项需要明确缓存时间、窗口单位、绑定失效及未知值语义，并避免管理读取触发模型或同步全池查询。最新 `FetchCredentialQuota` 在没有 provider 或声明式 `quota_probe` 时仍返回 501；`quota.fetch` 是宿主调用 provider 插件的方法，最新回调分发中仍没有通用 `host.quota.fetch`。它不是立即移除现有查询适配器的依据。[额度处理](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/api/handlers/management/plugin_quota.go#L59)、[host callback 分发](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/internal/pluginhost/host_callbacks.go#L147)。

## 实施顺序与边界

1. 修正全局代理继承，补充代理模式诊断；与 schema 兼容验证一起交付。
2. 接入 `host.auth.models`，统一后台模型目录和发送前检查，并保留旧宿主兼容路径。
3. 做账号同步与数据库写入基准，再实施分层同步；同时优化面板过滤读取。
4. 记录响应钩子性能，推进宿主的仅头观察能力，再决定是否替换现有回调。
5. 视统一面板需求决定 QuotaProvider；不把 API 路径重命名作为主要优化项目。

最新 API 仍未提供本项目可直接使用的“精确账号凭据代际 + 前台执行原子租约”。`host.auth.get` 仍读取物理凭据 JSON，不能保证 token 总是宿主内存中的最新值；`host.auth.get_runtime` 也不能单独证明上游账号身份。继续保留发送前后身份核对、结果未知不重发及 OAuth 由 CPA 管理的边界。

README、V2 安装文档和接口参考清单仍有 rc.18 / 固定旧提交描述，而当前源码为 rc.19。后续实施应同步更新兼容矩阵，分别标明插件版本、宿主版本、原生 ABI、RPC schema、数据库 schema 和单项回调支持状态。

## 本次验证

- `make check`：全量 Go 测试和 `go vet ./...` 通过。
- `make v2-smoke`：35 项原生假宿主测试、242 项 Node 测试通过。
- 额外复用假宿主验证协商：宿主提供 8/7/6 时，桥接层分别选择 7/7/6；前两者均声明省略 before/after 请求正文。
- 初次测试受到沙箱本地 socket 权限限制；在获准的扩展执行环境重跑后通过。
- 上述结果验证当前项目及模拟协商，不等于已完成 v8.0.47 真实宿主集成或生产性能测量；本文收益判断均为接口与源码分析，未给出实测降幅。
