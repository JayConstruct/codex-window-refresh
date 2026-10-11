# CPA 宿主接口核对与插件适配设计

状态：已完成官方源码只读核对，插件原型已构建并通过 C 假宿主加载；未编译完整宿主、未请求真实账号或部署。当前实现见 [插件入口](plugin.md) 与 [CPA 适配器](cpa-adapter.md)。接口结论适用于下列固定提交，不能自动推广到旧版、Plus、Home 或其他 fork。

## 1. 来源与版本边界

官方目标为 [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/README.md)，源码固定在 [`3de4e248f6bb12fc7dfafb183404f3b248578922`](https://github.com/router-for-me/CLIProxyAPI/commit/3de4e248f6bb12fc7dfafb183404f3b248578922)（2026-10-10T17:16:32Z）。其 [go.mod](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/go.mod#L1) 模块为 `github.com/router-for-me/CLIProxyAPI/v8`，Go 为 1.26.0。

[pluginabi](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginabi/types.go#L5) 定义原生 C ABI 版本 1，RPC schema 版本 6。Schema 2 增加请求完成回调，Schema 4 增加上游 WebSocket 事件观测。插件是动态库，Unix loader 查找 `cliproxy_plugin_init`，核对 ABI，并要求 call/free_buffer 函数完整。官方提供 [Go 动态库示例](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/simple/go/main.go)，可作为后续壳层依据。这是原生 C ABI + JSON RPC，不是 Go `plugin.Open` 的同工具链 ABI。

社区 `codex-keepalive` 的 README 建议 v7.2.142，但当前核对的是官方 v8 提交。旧版本最低兼容范围尚未逐 tag 验证；第一版应锁定实际部署 commit，不以社区文档代替官方接口来源。源码中的 `/v0/management` 和 `/v8/management` 是管理路由版本，不等同于模块 ABI/schema。

实现核对时，官方完整仓库曾拉取到 `/tmp/codex-window-refresh-official`，并固定到上述提交。本地工作区还保存过部分接口快照；公开仓库仅保留 [官方接口来源清单](../third_party/cliproxyapi-reference/MANIFEST.md) 和 MIT LICENSE，参考源码由 `.gitignore` 排除，不是插件构建依赖。未编译或运行完整官方宿主。

## 2. 已核实接口矩阵

| 设计需求 | 官方接口 / 数据 | 已核实能力与限制 | 推荐适配 |
| --- | --- | --- | --- |
| 列出 Codex 账号 | `host.auth.list`；[HostAuthFileEntry](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L752) | 有 ID、AuthIndex、Provider、Disabled、Unavailable、RecentRequests；无 5h/周窗口，也无凭据代际 | 以 ID 绑定生成，AuthIndex 定位查询；只持久化非敏感账号信息 |
| 执行前再核对账号 | `host.auth.get_runtime`；[回调实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/auth_callbacks.go#L95) | 返回同一 HostAuthFileEntry；不是原始运行时 Auth 的全部字段 | 每次发送前核对存在、禁用、provider 与身份；不把 UpdatedAt 当凭据代际 |
| 获取账号凭据 | `host.auth.get`；[实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/auth_callbacks.go#L68) | 根据 AuthIndex 读取物理凭据 JSON；含敏感 token；不保证就是内存中最新凭据，不是原子刷新接口 | 尽量不使用，若额度适配器需要，只在内存处理，禁止日志/存储原 JSON |
| 精确账号生成 | `host.model.execute` / `host.model.execute_stream`；[请求定义](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L672) | 有 AuthID、ForcedProvider、协议、模型、ProxyURL；[执行实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/model_execution.go#L117) 将 AuthID 固定为 pinned auth | 使用 HostAuthFileEntry.ID，不能把 AuthIndex 直接误填成 AuthID；ForcedProvider=codex；流完整消费和关闭 |
| 生成后的额度头 | [HostModelExecutionResponse](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L700) / stream response Headers；[UsageRecord.ResponseHeaders](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L1544) | 可读取响应头，但字段是否存在由上游和路径决定；HTTP 200 不足以确认窗口激活 | 缺字段转主动查询，不能推算 now + 5h |
| 正常业务开始 | `request.intercept_before`；[处理链](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers_execution.go#L64) | 已通过入口认证，在上游账号选择之前；有 UUID RequestID、TraceID、caller_scope | 建立 RequestID 活动表，按可信 caller_scope 去重；不修改请求 |
| 账号选定与前台占用 | `request.intercept_after`；[定义](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/executor/types.go#L101) | 在选账号后、上游执行前；[宿主写入](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/auth/conductor_execution.go#L1748) selected_auth_id/index | 记录 RequestID → 账号；重试/故障转移同 RequestID 可能多次 after，不能无限递增 |
| 业务终止 | `request.complete`；[生命周期实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers_interceptors.go#L119) | UUID 关联；成功、失败、拒绝、取消；sync.Once；异步投递，无持久重放 | 按 RequestID 幂等释放活动；不把“成功完成”直接视为新窗口证据 |
| 完成使用与额度 | `usage.handle`；[适配实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/adapters_usage_translation.go#L165) | 有 APIKey、AccessProvider、AuthID、AuthIndex、ResponseHeaders；多个上游尝试可能有多个记录 | 补充结果和额度，按 RequestID 分类；未知记录只更新可关联额度，不增加人数 |
| WebSocket 额度事件 | `websocket.response_event`；[定义](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L1277) | 有 AuthID、EventType、Payload；Schema ≥4；[Codex 解析源码](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/runtime/executor/helps/codex_quota.go#L28) 支持 primary/secondary窗口及 additional limits | 只解析明确额度事件，不保存会话载荷；按 window_minutes 分类，不认定 primary 必然为 5h |
| 主动额度查询 | 官方 `quota.fetch` / `sdk/pluginhost.FetchQuota` 与管理 quota/fetch | `quota.fetch` 是宿主调用 QuotaProvider 插件的方法；嵌入 Go API 不是动态库的 host callback；当前没有 host.quota.fetch RPC | 单独做 QuotaProbe 适配器或建议宿主新增受账号绑定的查询回调 |
| 管理额度查询 | [管理 quota/fetch](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/plugin_quota.go#L58) | 根据 provider 插件或 auth.metadata.quota_probe 执行；没有配置则 501，不保证 Codex 默认具备 | 部署时确认额度提供插件及字段映射；不能默认宿主自带上游 wham 查询 |
| HTTP 查询与账号代理 | `host.http.do` / do_stream；[实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_callbacks.go#L187) | 当前回调采用 newHTTPClient(nil)，HTTPRequest 不含 AuthID/ProxyURL；不能假定自动注入目标账号 token或账号代理 | 通用 HTTP 仅用于已明确适配的查询，不把其当精确账号 HTTP 接口 |
| 复用宿主管理查询 | [api-call](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/api_tools.go#L63) | AuthIndex + $TOKEN$，有账号/全局代理继承及 token 解析；是管理认证 HTTP 接口，不是原生 host callback | 如采用桥接模式，管理密钥仅由宿主/sidecar安全注入；不要存进业务配置或页面 |
| 额度“重置” | `host.routing.reset_cooldown`；[实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/routing_callbacks.go#L13) | 仅清本地路由冷却，不能重置上游 5h/周窗口 | 本插件不调用；不调用额度付费 reset/购买 credits |
| 管理页面 | ManagementAPI / ResourceRoute；[定义](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L1405) | 管理接口受宿主认证；资源路由明确不受 management auth | 静态页面可公开，账号状态、配置、日志均通过受认证管理 API |

## 3. 可信用户数：可直接使用 caller_scope

已核实链路：

1. [原生 config-access](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/access/config_access/provider.go#L93) 只有匹配已配置 key 才返回认证 Principal，原生 Principal 是该 key 原文。
2. [AuthMiddleware](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/server_middleware.go#L160) 将认证结果 Principal 存入 `userApiKey`，并写入 usage 上下文；外部 frontend-auth 插件也可提供已认证 Principal。
3. [requestExecutionMetadata](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers.go#L248) 调用 `requestCallerScope`；后者从已认证 `userApiKey` 读取值，不是直接相信客户端 user 字段。
4. [CallerScope](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/session/identity.go#L228) 使用带命名空间的 SHA-256 生成不可逆身份，存入 `metadata.caller_scope`，可在 before 时得到。
5. 完成使用记录的 APIKey 使用同一认证 Principal。必要时在内存计算相同 caller scope，用于比对；不能落盘原 key。

第一版默认统计口径为“近期已认证客户端身份数”。原生 key 模式是去重 key 数，不是自然人数量；共享 key 的多人只能算一个，多 key 的同一个人会算多个。若部署 frontend-auth 返回稳定用户 ID，可按用户口径统计。不同 auth provider 若可能输出相同 Principal，现有 caller_scope 没有 provider 命名空间，应要求配置映射/保证 Principal 唯一，不能假定全局唯一。

活动输入白名单是正常生成请求，不包括 model list、token count、管理请求、健康检查。读取 metadata.generate=false 时排除非生成；通过 request_path/source_format 与部署端点列表确定实际业务入口。不能从 session_id、会话数量或 prompt 自称 user_id 推导独立用户。

## 4. 正常业务与插件流量隔离

[内部 model 执行](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/model_execution.go#L104) 强制标记 InternalSource=true，[addModelExecutionSourceMetadata](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/model_execution.go#L285) 写入：

```text
metadata.source = "plugin_host_model_callback"
```

这不是外部客户端 header。其他插件的 host model 回调即使继承了用户认证上下文，也必须按这个宿主标记排除活动统计。调用方自己的请求/响应/流、生命周期和 WebSocket观察会被[skipPluginID 路径](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_callbacks.go#L422) 跳过；因此本插件自身唤醒的开始、完成、额度结果由调度操作直接记录，不等待观察回调。

usage.handle 不在上述 skip 观察机制中；[UsageRecord.Source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/adapters_usage_translation.go#L202) 来自 usage 的 upstream source，不能冒充 metadata.source。必须先在 before 创建请求来源表，然后只对已登记为普通业务的 RequestID统计人数；未知 UsageRecord 不增加人数。查询直接 HTTP 无模型 before 记录，也自然不计入。

如果其他后台程序用普通 /v1 入口和同一个业务 key 请求，它从 CPA 视角就是业务请求，现有机制不能识别人是否操作。需要分配专用后台 key 并配置排除，或让可信入口提交机器任务分类。不能用客户端随意可伪造的“不是用户” header 影响名额。

## 5. 生命周期活动表与前台占用

建议内存表：

```text
request_id → caller_scope, classification, started_at,
             observed_accounts, current_selected_account, completed_at
caller_scope → active_request_ids, last_real_activity_at
account_id → active_request_ids
```

before 对同 RequestID 幂等登记一个活动；after 对同 RequestID 更新账号绑定，不能每次回调加一个计数；complete 按 RequestID 清除全部记录到的占用，并更新真实活动时间。同一用户长请求执行期间计入 U，终止后保留到活动 lookback 到期。不同 RequestID 的重试依靠 caller_scope 去重，不会增大 U。

complete.Metadata 保存初始 reqMeta 的引用，正常路径可能携带 selected_auth_id，但若其他拦截器调整路径引发 metadata clone，不能依靠 complete 字段必然完整。以本插件 after 时保存的映射为准，缺失账号信息时保守标为 unknown。

官方 tracker 的 sync.Once 保证宿主当前流程只创建一次终止事件，并不提供断线/卸载后的插件持久消息保证。加载时活动内存表为空；长请求跨插件重载可能没有 before。建议将观测能力状态暴露给调度器：重载进入观察恢复阶段，只有活动表已恢复/状态足够可信后才放行普通唤醒。固定 TTL 不能把仍执行的长请求直接认定完成；超长条目应标记异常、保留活动占用并触发人工可见诊断/宿主对账。若宿主没有活动请求快照，不能承诺可精确恢复所有前台请求。

现有回调仅观测，并没有“锁账号使正常业务暂不进入”的原子准入 API。本地锁只互斥本插件任务，仍可能在前台检查完成后、新用户请求开始前的间隙发送。第一版可实现观测后跳过和最终再查；若要求严格不与普通请求同时触发，需新增宿主原子账号执行租约/准入接口。不能用接管 scheduler.pick 来暗中改变正常路由。

## 6. 额度与身份适配边界

宿主 Auth 对象内部可能记录更多额度与 credential version，但原生 host.auth.get_runtime 没有暴露这些字段。插件维护自己的稳定本地窗口基线是可行方案；首次部署或重启先由可信观测恢复，不能将 RecentRequests 或成功计数转成额度。

凭据代际不能用 UpdatedAt/文件 mtime，因为 OAuth 自动刷新也改变它们。可使用“宿主稳定 ID +上游 account_id 的本地摘要”识别账号替换；若无法可信取得实际 account_id，则文件替换/重新登录视为需要重新建立观察基线。账号被同路径替换后生成前的强一致核对仍需宿主增补 credential_generation 字段。

建议选择一个明确的 QuotaProbe 模式：

- **官方 quota provider桥接**：已有可靠 Codex provider或配置 quota_probe时复用其标准化结果。动态库当前无 host quota callback，桥接必须显式设计，不得编造 SDK 方法。
- **管理 api-call桥接**：目标 AuthIndex、$TOKEN$、确定的额度 URL；复用宿主管理凭据解析/代理，返回原始额度只在内存解析。部署需验证管理认证、token刷新、账号 header和代理行为。
- **插件独立 HTTP**：从内存凭据获取目标账号 token，配置正确代理，自己承担刷新和查询字段适配；代价更高，默认不选。不把 host.http.do 误当自动绑定账号能力。
- **宿主小扩展**：增加 host.quota.fetch(auth_id/generation)、认证主体摘要、请求来源和前台原子准入。对纯插件第一版这属于可选后续，未实现时诚实声明限制。

上游 Codex 额度 URL、响应 schema以及“周重置同时重置5h”目前没有由此宿主 SDK 契约保证。用户规则采用联合重置假设，但实际观测冲突仍必须停留等待；窗口解析以 window_minutes 与 reset_at/reset_after、allowed、used_percent等证据为准，缺字段不构造余额和窗口。

## 7. 部署前必须验证的能力

以下不是已完成的运行结果：

1. 目标运行版本、CGO 插件支持、ABI/schema协商及启停/quiesce行为。
2. 精确 AuthID 在实际 normal/home/remote模式均不回退其他账号；模型与协议可用，代理继承和 token自动刷新正确。
3. 普通 HTTP、SSE、上游 WebSocket三条业务路径都有可信 caller_scope、source排除、selected_auth_id、生命周期完成；失败/取消/故障转移不重复增加人数。
4. quota响应提供短期和周字段；额外模型额度与基础额度不混用；已激活与零用量占位窗口能区分。
5. 唤醒成功和结果不确定时都可以主动查询同一账号；额度更新可能滞后，验证重试不增加生成请求。
6. host model内部回调的响应是否保留所有额度头；如缺失，应落入查询路径。
7. 账号删除/同名替换/自动token刷新能够区分；不能因为普通OAuth刷新丢失全部随机任务。
8. 活动记录丢失、插件热更新和宿主重启后采用保守恢复；严格前台互斥能力未提供时展示为观测能力限制。

这些验证不需要改核心规则算法；它们是适配器能力门槛。能力未知时应暂停依赖该能力的自动规则并显示原因，不能用伪造用户数、凭据猜测或固定倒计时替代。

