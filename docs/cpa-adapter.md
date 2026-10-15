# CPA JSON 回调适配器

`internal/cpa` 使用本地核对的官方 v8 源码提交 `3de4e24` 的 JSON 契约，不依赖官方 SDK 的 Go 模块。原生 ABI 注册由 `internal/plugin` 和 `cmd/plugin` 完成。本适配器的测试使用假回调与内存 HTTP transport；实现期间未读取真实账号文件、查询真实额度或发送真实生成请求。

## 回调接口

```go
type Callback interface {
    Call(context.Context, string, any, any) error
}

host := cpa.NewHost(callback, quotaReader)
```

`Host` 实现核心 `core.Host`，另提供 `SetBaseline(core.CoreState)` 与 `ActivityAccounts()`。适配器只消费以下官方回调：

| 回调 | 用途 | 关键 JSON 字段 |
| --- | --- | --- |
| `host.auth.list` | 列举 Codex 宿主账号 | `files[].id/auth_index/provider` |
| `host.auth.get_runtime` | 核对精确账号绑定与可用状态 | 请求 `auth_index`；响应 `auth.id/auth_index/provider/disabled/unavailable` |
| `host.auth.get` | 核对身份并临时读取查询凭据 | 请求 `auth_index`；响应 `json.account_id/type/access_token/expired/proxy_url` |
| `host.http.operation_open` / `host.http.do` / `host.http.cancel` | 查询上游额度并取消超时操作 | `operation_id`, GET 固定额度 URL, `headers`；响应 `StatusCode` 和 base64 `Body` |
| `host.model.execute_stream` | 发送精确账号生成 | `auth_id`, `forced_provider`, `entry_protocol`, `exit_protocol`, `stream`, base64 `body` |
| `host.model.stream_read` | 消费模型输出 | 请求 `stream_id`；响应 base64 `payload`, `error`, `done` |
| `host.model.stream_close` | 显式释放模型流 | 请求 `stream_id` |

官方对应定义在 `sdk/pluginapi/types.go`、`internal/pluginhost/auth_callbacks.go`、`internal/pluginhost/host_model_stream_callbacks.go`。`host.stream.close` 用于另一类 executor 输出流，不能代替模型流关闭。原生 dynamic callback 的 host context 已携带插件身份，后台工作不借用已结束的请求 callback scope。模型流取消时，适配器发出独立 stream_close 取消宿主上游，再等待取消 watcher 结束后返回，避免卸载后遗留调用。原生同步 C 回调无法被 Go deadline 强行中断；若宿主回调本身完全不返回，关闭过程仍需安全等待。

## 精确账号与代际

生成使用宿主稳定 `id` 作为 `auth_id`，同时锁定 `forced_provider=codex`；管理额度查询使用同一绑定的 `auth_index`。发现、查询前后、生成前都会核对 runtime 信息与实际 `account_id`。

额度查询前后的绑定核验同时缓存 CPA 的可用状态，协调器在处理查询结果时只同步该账号，并随额度结果提交、发布。查询失败也同步成功核实的 CPA 状态，保留原额度及观测时间；本地状态核验失败则保留最后已核实的状态。新凭据代际会使旧额度失效，旧请求结果不会落入新账号。状态同步复用本次核验结果，不额外扫描账号池或发送模型请求，也不根据额度成功与否修改 CPA 的禁用或暂不可用标记。

代际是 `SHA256("codex-account-v1" + 分隔符 + 宿主 ID + 分隔符 + 实际 account_id)`。不使用 token、email、文件时间、`updated_at` 或 OAuth 刷新时间。OAuth 正常续期不会丢失窗口；同一宿主 ID 换成另一真实上游账号会阻止旧任务生成。

`host.auth.get` 返回的凭据 JSON 只在调用栈里用于解码 allowlist 字段，随后清理原始字节。账号 token 不进入状态、配置、日志或错误。缺少 `account_id`、runtime-only 账号或无法核对绑定时返回 `account_identity_unavailable`，不会用名称或 email 猜测。清单发现中的单账号绑定错误会隔离为禁用占位账号，其代际使用独立不可用命名空间，旧额度和活动绑定失效；该账号不缓存可发送绑定、不能发送模型，手动查询返回固定身份错误。健康账号不受影响。重复宿主 ID 的冲突项全部禁用，缺少 ID 的条目跳过并记录安全诊断；全局清单读取失败、取消及超时仍使整个刷新失败，不发布部分清单。宿主无法证明同一上游账号删除后重新添加的独立凭据生命周期；首版代际明确采用上游账号身份口径。

目前宿主没有与前台生成联合原子锁定的接口，`ForegroundGuard=false`。生成前核对无法彻底消除核对之后、真实宿主执行之前的账号修改或前台请求竞态。

## 默认宿主额度查询

`QuotaReader.Read(ctx, AuthBinding)` 可注入受信实现。默认 `NativeReader` 不需要 CPA 管理地址或管理密钥环境变量；包装配置 `quota_transport` 留空或设为 `host` 即可。旧安装保留的管理桥字段不影响宿主模式。

每次查询通过 `host.auth.get` 重新读取当前账号凭据，只解码身份、访问 token、有效期及代理字段。再次核对 auth_index、实际 account_id 和代际，然后 GET 固定的 `https://chatgpt.com/backend-api/wham/usage`。查询前后仍由 Host 核对账号状态与身份，不把 token 刷新当成账号更换。

- 无账号专用代理时调用 `host.http.do`，沿用宿主全局代理。通过 `host.http.operation_open` 创建操作，超时或取消时使用 `host.http.cancel` 终止。
- 账号配置 `proxy_url`（兼容 `proxyUrl`、`proxy-url`、`proxyURL`）时，使用私有 HTTP transport 支持 HTTP、HTTPS、SOCKS5、SOCKS5H 和显式 direct/none，不继承系统环境代理；多个非空代理字段须一致，否则拒绝查询。禁用重定向，响应限制为 2 MiB。按规范化后的完整代理配置复用连接，代理认证信息不同则隔离；最多保留 16 个 transport，空闲连接 90 秒到期。每次请求仍重新读取账号 token，不缓存认证 header。关闭插件时等待工作线程结束，再释放连接；配置失败也释放已创建的连接。
- 不调用模型执行、登录、`host.auth.save` 或 refresh-token 接口。CPA 保持 OAuth 续期的唯一所有者。凭据文件明确过期或上游返回 401 时报告 `quota_credential_expired`，等待 CPA 更新或用户重新登录后重试；不通过模型请求更新凭据。
- 物理凭据文件可能暂时落后于宿主内存。插件每次重新读取，但不能承诺文件 token 始终最新；可选择管理桥以复用宿主内存中的 token 解析。
- token 只用于当前调用，不进入配置、状态、日志或错误。上游、代理和回调异常只发布固定分类，不附带响应正文或凭据。

`QuotaAdapterReady` 在宿主回调配置具备时为 true，并不预先验证网络或每个账号的 token。`quota_bridge.mode=host`、`status=host_callback` 标识此方式。单账号手动任务的 `last_error` 可解释过期凭据、缺少凭据、无效代理和查询失败。

`quota_bridge.host_http_status` 单独显示宿主操作接口验证结果：`unchecked`、`operation_open_verified`（真实查询已成功创建操作）、`verified`（显式检查已创建并取消操作）、`operation_open_failed`、`operation_cancel_failed` 或 `callback_missing`。读取能力接口只返回快照；显式 `POST /quota-connection-check` 才进行本地创建/取消检查，不读取账号凭据、不调用 host.http.do 或模型回调。接口检查失败不关闭全部账号查询，因为账号专用代理路径不依赖宿主 HTTP 操作。原生同步 C 回调若完全不返回，Go 的 deadline 无法强行中断它，仍需宿主恢复或重启。

## 可选管理桥

明确设置 `quota_transport: management` 后使用原 `BridgeReader`：

- `management_url` 与 `management_key_env` 必须成对提供。实际管理密钥来自 CPA 进程环境，不放进规则文件。
- 支持 CPA 服务基址、`/v0/management`、完整 `/v0/management/api-call` 或 `/v8/management/requests/api-call`。
- POST 管理 `api-call`，正文使用绑定的 auth_index，上游 GET 仍固定为 wham/usage，Authorization 使用 `$TOKEN$`。CPA 解析账号 token，并继承账号/全局代理。
- 管理与上游状态均须为 200；最多读取 2 MiB，禁止转送管理凭据到重定向地址。

两种模式均设置官方额度面板使用的 Codex User-Agent、Content-Type 与账号 header。总查询 deadline 来自 `execution.quota_read_timeout`，默认 15 秒，允许 1 秒至 5 分钟；插件自有 HTTP client 的安全上限为 5 分钟。CPA 管理 api-call 还可能有宿主自己的请求超时，插件无法扩展该宿主限制。

## 查询失败诊断

查询器只返回固定错误代码及脱敏结构 `code/stage/http_status/elapsed_ms`，不携带远程原始错误、响应正文、管理地址或代理凭据。协调器再次校验代码和阶段白名单，只接受 100–599 的 HTTP 状态，用本地实测时间发布耗时。单账号及批量任务通过 `scheduler.quota_query.diagnostic` 发布最近一次失败；自动与手动查询的最近失败通过 `scheduler.last_quota_diagnostic` 发布，仅保存在运行时内存，后续成功不删除历史失败记录。

| 固定代码 | 含义 |
| --- | --- |
| `quota_host_operation_failed` | 宿主 HTTP 操作创建或取消失败；stage 区分 operation_open / operation_cancel |
| `quota_host_http_failed` / `quota_native_query_failed` | 宿主 / 账号代理请求失败 |
| `quota_query_timeout` / `quota_query_cancelled` | 超时 / 取消，保留实际失败阶段 |
| `quota_credential_expired` | 文件标记过期或上游 HTTP 401；不代表已自动刷新 |
| `quota_upstream_forbidden` / `quota_upstream_rate_limited` | 上游 HTTP 403 / 429 |
| `quota_upstream_server_failed` / `quota_upstream_http_failed` | 上游 5xx / 其他异常状态 |
| `quota_management_auth_failed` | 管理 HTTP 401/403；与上游认证失败分开 |
| `quota_management_endpoint_missing` / `quota_management_http_failed` | 管理 HTTP 404/405 / 其他异常状态 |
| `quota_response_invalid` / `quota_response_too_large` | 非法 JSON、缺少或冲突的管理状态字段 / 响应超限 |
| `quota_response_unrecognized` | 有效 JSON 的额度窗口缺失、矛盾或无法识别 |

管理桥兼容 status_code/statusCode，二者同时存在须等值。失败保留已有额度；手动任务不自动重试，定期查询继续沿用退避策略。管理桥复用 Codex 运行时 token，不保证按需刷新 OAuth，物理文件与内存差异仍是部署边界。

桥缺少地址或环境密钥时才会 `QuotaAdapterReady=false`。`quota_bridge.mode=management`，status 继续区分 `missing_management_url`、`missing_management_key_env`、`missing_management_key`、`invalid_management_url` 与 `configured`。诊断不返回地址或密钥，也不发送网络探测。接口来源见官方 `internal/api/handlers/management/api_tools.go`。

## 额度证据

解析 `rate_limit.primary_window/secondary_window`，以及官方 quota event 的 `rate_limits.primary/secondary`、`rateLimit` 与 camelCase 字段。5 小时与周字段按 `limit_window_seconds=18000/604800` 或 `window_minutes=300/10080` 识别，不能仅按 primary/secondary 的位置判断。主池以外的附加额度不代替普通 Codex 窗口。

`used_percent` 必须为 0 到 100 的有限数；保留 `remaining=100-used_percent`，单位 `percent`。优先 `reset_at`，否则使用 `reset_after_seconds`/`reset_after`。没有有效持续时间、用量或重置证据时保持未知或报告未识别，不填伪造倒计时。

同字段的 snake_case、camelCase 和 legacy 别名同时存在时必须一致；主次窗口与顶层 rate-limit 对象的重复表示只接受归一化后等值的内容。持续时间的秒数与分钟数也必须一致，不能任选一个来产生可发送结论。相对倒计时按对应窗口限制为 5 小时或 7 天加 60 秒，与绝对截止时间的上界一致。

同时提供绝对截止时间与相对倒计时时，允许 60 秒的采样、整数舍入和网络延迟差异，但不会用这个容差放行到期：绝对时间已经到期而相对倒计时仍为正时，整个观测报告未识别；请求传输时刚好跨过边界也可能触发这个保守判断，后续查询再确认。相对倒计时为零时允许绝对时间处于过去，因为上游可能把过期倒计时钳制为零。明确非法的 `reset_at <= 0` 不回退到相对时间猜测。

- 明确 past reset boundary 分类为 `expired`。
- 未来 reset boundary 且非零用量分类为 `active`。
- 五小时窗口的服务端剩余秒数严格在 `0 < remaining < 18000` 时，即使用量为零也分类为 `active`，支持少量请求后的零百分比用量。优先使用服务端显式相对倒计时；只有绝对截止时间时，按截止时间与观测时间的整 Unix 秒差判断，避免观测时间的小数秒把完整五小时误判为缩短。若显式倒计时仍为 18000 秒，不因绝对时间的采样差异认定启动。
- 其余零用量与未来 reset boundary 分类为 `unknown`，周窗口不应用上述五小时规则。如果是主动查询，且同账号、同代际的现有 active 证据和 baseline 都具有完全相同的未来截止时间，则只保留该已证实的 active 周期，不声明新周期。即使本地旧 baseline 到期，也不能据此声称上游新字段是未激活占位。
- 当前解析器不凭零用量生成 `inactive`。从 rc.24 起，已有稳定五小时基线确实到期且存在对应到期意图时，允许使用到期后新查询的满额五小时占位值执行该次唤醒，仍检查请求权限、新鲜度、人数、时段及发送抑制。占位值保持 `unknown`，不产生新基线或启动倒计时；没有历史到期依据时继续等待或显式手动唤醒。已确认的未来基线及缩短倒计时继续阻止提前发送，送达未知时满额占位值不能确认启动或触发补发。

生成 response headers 的 `X-Codex-Primary/Secondary-*` 和 `codex.rate_limits` SSE 帧也会转换为独立额度观测。来自响应头的旧证据不会因为后续帧到达而自动刷新时间；生成缺少额度信息时仍由运行时安排主动查询。HTTP 200、`[DONE]` 或只有文本 delta 不代表生成完整完成；必须完整消费模型流并收到 `response.completed`，没有 stream 错误才返回 `SendCompleted`。流打开或读取失败会返回 `SendUnknown`，避免错误地声称请求未送达。

## 正常请求返回的额度

原生 schema 6 注册 `response_interceptor`、`response_stream_interceptor` 和 `websocket_response_observer`。对应 RPC 为 `response.intercept_after`、`response.intercept_stream_chunk`、`websocket.response_event`；名称由官方 `sdk/pluginabi/types.go` 和 `internal/pluginhost/rpc_client.go` 核对。HTTP 使用 `ResponseHeaders` 中的 `X-Codex-Primary/Secondary-*`；流式只读取 `ChunkIndex=-1` 的 header-init，不把后续正文 chunk 重复携带的旧头当作新观测。WebSocket 使用上游原始 `Payload` 的 `codex.rate_limits`，也接受 `error.headers` 中的额度字段，兼容小写头名并拒绝矛盾大小写变体。

`request.intercept_after` 捕获 `RequestID → 已核对的 Codex 账号 ID 与凭据代际`。响应可缺少最佳努力的 Metadata，但明确提供的 `selected_auth_id` 必须与绑定一致；WebSocket 的 `Provider=codex`、`AuthID` 和可选 `EventType` 还需与绑定和 payload 一致。重试换号会替换绑定，换到未支持账号时清除旧绑定；刷新宿主清单发现账号代际变化后，旧请求响应不会套用到新账号。宿主仍未提供与账号替换原子同步的凭据代际，当前核对依据已发现的宿主清单，无法声称替换瞬间强一致。

为兼容异步完成通知先于最终响应拦截，完成后的绑定保留 2 分钟；未完成绑定最多保留 10 分钟，总计最多 4096 项，过期或容量淘汰后回退主动查询。只保存请求与账号绑定，不保存提示词、响应正文、凭据、stream history。响应观察回调不做磁盘/网络 I/O，只向协调器提交结构化额度，返回空修改对象 `{}`，原响应头、正文及 stream chunk 原样保留。无额度、绑定未知、畸形或无关数据均忽略，不影响正常请求。客户端正文中的账号或额度字段不参与识别；插件自身生成通过独立执行适配器观察，避免重复收集。

新来源为 `cpa_response_headers`、`cpa_websocket_quota`，观测分类为 `execution_response`。字段仍独立接受，缺失周窗口不会清除已有周额度；旧查询不能覆盖已接收的新响应。完整活跃窗口证据可推迟普通查询，实际唤醒前查询与未完成操作验证仍保留。被动更新不要求管理额度桥可用，但自动唤醒仍要求桥就绪。

## 用户活动

`ParseActivity(method, payload, accounts)` 只接收原生宿主认证回调 payload。官方 lifecycle 外层字段为 PascalCase `RequestID/Metadata/CompletedAt`，Metadata 子字段为 `caller_scope/source/selected_auth_id`。请求正文里伪造的同名字段不参与统计。

- `request.intercept_before`：只有宿主 `caller_scope` 存在才发出 started 事件。
- `request.intercept_after`：按宿主 `selected_auth_id` 关联已核对的账号与代际；没有该字段则不能靠模型名猜账号。
- `request.complete`：按 RequestID 结束原有活动绑定；允许 completion 的 Metadata 不完整。
- `source=plugin_host_model_callback`：整个生命周期排除，不把插件生成计入真实用户活动。

开始/结束幂等、重试更换账号与 in-flight 去重由核心 reducer 按 RequestID 处理。当前适配器只实现 `host_caller_scope` 口径；其他严格身份来源应在原生入口能力校验时拒绝，不能声称 caller_scope 是独立自然人。

`PassThrough(method,payload)` 保留普通请求的原 Headers 与原 Body，返回无终止的响应。活动观测不改写正常用户请求。

## 已验证

配置与代际测试覆盖 OAuth 刷新、真实账号替换、精确 `auth_id`、模型格式映射、终端 completion、分片 SSE、异常流显式关闭、管理密钥缺失、token substitution、主次窗口互换、零用量占位、活动排除和正常请求内容原样返回。额度一致性测试覆盖矛盾别名、矛盾重置时间、各窗口的倒计时上界、等值别名及采样延迟，确保畸形响应不会提供到期或 permission 证据。

## 反重力短句生成

反重力账号仅加入独立的生成账号目录，Codex 的 `Accounts` 清单保持专用于额度和唤醒调度。目录复用 `host.auth.list` 与 `host.auth.get_runtime` 本地回调，以稳定宿主 ID、邮箱及项目 ID 的摘要校验绑定，不读取或保存反重力原始凭据。元数据字段参照本地固定宿主快照的 `HostAuthFileEntry`。

生成通过 `host.model.execute_stream` 固定 `forced_provider: antigravity` 和所选 `auth_id`；`entry_protocol` 与 `exit_protocol` 均为 `openai-response`，使用宿主转换后的 Responses 文本事件复用现有候选解析。模型名原样交给 CPA，只能使用所选账号在宿主中已注册且可路由的模型。流读取、取消、关闭和完成事件校验沿用原有宿主回调生命周期，不自行连接上游。
