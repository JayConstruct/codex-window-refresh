# 原生插件入口与管理接口

`cmd/plugin` 为 Linux / CGO 的 CPA 原生动态库入口，C ABI 版本 1，注册 JSON-RPC schema 6。ABI 和字段大小写已对照固定版本的官方 `sdk/pluginabi/types.go`、`sdk/pluginapi/types.go`、`internal/pluginhost/rpc_schema.go` 与原生示例核对；未引入 CPA SDK 模块依赖。

```sh
GOCACHE=/tmp/codex-window-refresh-go-cache CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o /tmp/codex-window-refresh.so ./cmd/plugin
```

该命令生成 `.so` 与 C 头文件。将动态库放入配置的 `plugins.dir`，文件名使用 `codex-window-refresh.so` 或 `codex-window-refresh-v0.1.0.so`；官方宿主从文件名识别稳定 ID 与可选版本。不要增加 `lib` 前缀，否则会成为另一个插件 ID。 首版支持手动放置动态库；插件商店附加的 `store` 元数据尚未接入严格包装解析，不能直接按商店安装方式配置。需要支持原生插件的同架构 Linux CPA 宿主。管理注册名称为 `codex-window-refresh`，版本 `0.1.0-rc.9`；声明 `request_interceptor`、`request_lifecycle_plugin`、`response_interceptor`、`response_stream_interceptor`、`websocket_response_observer` 和 `management_api`。该插件不替换正常账号路由。

## 宿主配置和策略文件

CPA 的插件配置包含宿主拥有的 `enabled`、`priority` 和四个包装字段，规则从独立文件读取。宿主会把前两项注入生命周期 `config_yaml`，入口按布尔和整数类型接收；不能混入策略规则字段。`config_file` 和 `state_dir` 必须为绝对路径；状态目录必须是运行用户拥有的 `0700` 目录，文件存储不主动改变已有目录权限。

```yaml
plugins:
  enabled: true
  dir: /absolute/path/plugins
  configs:
    codex-window-refresh:
      enabled: true
      priority: 0
      config_file: /absolute/path/config.yaml
      state_dir: /absolute/path/private-state
      quota_transport: host # 默认；不需要后台管理密钥
```

默认 `quota_transport: host`，直接使用宿主账号凭据查询额度，无需管理地址或管理密钥环境变量。旧配置中的管理桥字段可以保留，不影响默认查询。需要兼容桥时明确设置 `quota_transport: management`，并成对设置 `management_url` 和 `management_key_env`。密钥值由运行 CPA 进程的环境提供，不能写入包装 YAML、策略文件或调度状态。桥支持的地址和目标限制以 `internal/cpa/bridge.go` 的验证为准。未知包装字段、旧 RPC schema、混入规则字段、多个 YAML 文档均拒绝注册。测试宿主可省略 `enabled`、`priority`；真实 CPA 会注入两项。宿主 `enabled: false` 会关闭调度生成；`enabled: true` 不会把业务文件中的 `enabled: false` 改为开启。更改宿主优先级或启用开关不属于包装路径迁移，也不要求重启。

策略配置采用项目的 `config.example.yaml`。首版 CPA 适配器只支持 `activity.identity_source: host_caller_scope`，其他预留身份模式在注册和重新加载时明确拒绝。

`plugin.reconfigure` 和 POST `/reload` 重新读取该文件并验证；无效文件不会替换现有配置。修改包装路径、状态目录、查询方式或管理桥参数需进程重启，避免运行时迁移状态锁或账号绑定。

## 管理路由

以下完整路径通过 CPA 的 **已认证管理 API** 注册。请求方法必须匹配；不支持管理路径外的数据接口。

| 方法和完整路径 | 行为 |
| --- | --- |
| GET `/v0/management/codex-window-refresh/summary` | 实际 enabled（宿主与业务开关同时开启）、独立 business_enabled/host_enabled、运行状态、状态版本、账号数、预约数、错误、实际并发能力和调度汇总 |
| GET `/v0/management/codex-window-refresh/accounts` | 本地账号窗口、任务和证据；不含原始凭据 |
| GET `/v0/management/codex-window-refresh/config` | 当前生效的规则配置 |
| GET `/v0/management/codex-window-refresh/config-editor` | 磁盘策略与内容版本，用于并发编辑检查 |
| POST `/v0/management/codex-window-refresh/config` | 严格验证并原子保存策略、热应用；内容版本冲突返回 409 |
| GET `/v0/management/codex-window-refresh/capabilities` | 运行时宿主能力及 quota_bridge 本地配置诊断；不测试上游连通性，不返回管理密钥或地址 |
| POST `/v0/management/codex-window-refresh/reload` | 读取外部配置、验证后更新 |
| POST `/v0/management/codex-window-refresh/recheck` | 通知协调器重新检查；不能绕过窗口、用户名额、时间段或送达抑制规则 |
| GET `/v0/management/codex-window-refresh/snapshot` | 统一的已发布运行视图：`instance_id/snapshot_seq/summary/accounts/capabilities/config`；面板运行数据只读取此接口，配置文件草稿另读 config-editor |
| POST `/v0/management/codex-window-refresh/quota-query` | 无请求体或 `{}` 时批量查询；可选 `{ "account_id": "账号 ID" }` 仅查询该账号。手动查询始终允许，仍需要后台运行及额度桥就绪，返回 202、queued/already_queued 和稳定 `job_id`；不同目标忙碌时返回 409，未知账号返回 404；不创建唤醒任务或派发模型，进度位于摘要 scheduler.quota_query |
| POST `/v0/management/codex-window-refresh/manual-wake` | 必须传 `{ "account_id": "账号 ID" }`，仅排队该账号一次“查询后决定唤醒”任务；使用已保存的模型与消息，不接受模型覆盖或批量请求。自动唤醒可关闭，但 CPA 插件必须运行、账号未暂停、已保存模型且宿主能力就绪。返回 202，未知账号 404，参数错误 400，忙碌或待验证请求 409；进度位于 scheduler.manual_wake；不按缓存的账号 Enabled 拒绝排队，执行时查询最新额度并核对 CPA 状态，不满足条件时记录 blocked 及具体原因，不发送模型请求 |
| POST `/v0/management/codex-window-refresh/quota-connection-check` | 宿主模式下显式创建并取消本地 HTTP 操作，不读凭据、不执行 HTTP 或模型。返回 200 和固定 status，可带脱敏 error；管理桥模式返回 409，不支持检查的适配器返回 501 |

CPA 的 ResourceRoute 不做管理认证。菜单资源 `/v0/resource/plugins/codex-window-refresh/status` 与 `/panel.js`、`/panel.css` 只返回静态面板资产，HTML 不嵌入账号、配置、额度或运行状态。页面优先复用同源 CPA 已保存的管理凭据，无法复用时手动输入密钥，再通过已认证管理 API 查询与编辑；模块不持久化手动输入的密钥。资源使用限制同源脚本、样式与连接的 CSP。使用与保存行为详见 [网页面板](panel.md)。JSON 管理响应采用 schema 6 原始响应语义；Body 按官方 RPC 的 `[]byte` 序列化传输。

`/summary` 的 `scheduler` 字段用于实测时解释为何尚未唤醒账号：包含汇总发布时间 `observed_at`、近期身份数量 `recent_users`、身份来源 `identity_source`、容量 `capacity`（目标、活跃、预约和缺口）、阻止发送的账号数量 `hold_counts`、额度未知账号数 `quota_unknown_accounts`、前台请求占用账号数 `foreground_busy_accounts` 和下一次账号清单检查时间 `next_inventory_at`。人数按宿主认证的 caller scope 去重；共用 scope 的多个自然人只计一个身份。指标只包含汇总，不返回活动身份、请求 ID 或原始请求。

调度汇总从协调器发布的快照读取；查看管理状态不等待正在进行的额度查询或模型请求。`observed_at` 用于判断快照是否仍然新鲜。字段通过可选 `SchedulerDiagnostics() any` 接口提供，其他 Engine 实现可继续使用原有接口；未提供该接口时摘要不包含 `scheduler`。

`scheduler.quota_query.diagnostic` 提供本次手动任务最近失败的 `code/stage/http_status/elapsed_ms`；`scheduler.last_quota_diagnostic` 提供运行期间最近一次额度查询失败。两者不持久化，仅包含固定分类及本地耗时；返回的快照不会暴露可变的内部诊断对象。`/capabilities` 的 `quota_bridge.host_http_status` 是已有宿主接口检查结果，读取它不会主动检查网络。

手动额度任务的可选 `timing` 同时出现在 `scheduler.quota_query` 和对应 `scheduler.jobs[].quota_query` 中，提供毫秒值 `queued_ms`（等待工作线程）、`inventory_ms`（清单或目标身份刷新）、`query_ms`（额度读取与应用）、`commit_ms`（状态提交）、`total_ms`（接受任务至完成提交）。采用本地单调时钟，完成后固定，不计浏览器轮询、响应传输或渲染；各阶段不是全部执行开销的穷尽分解。数据只保存在内存，不包含 token、代理或上游响应正文。

### 快照与任务同步协议

`/snapshot` 从同一次原子加载取得已发布账号、调度摘要、任务、能力及生效配置；不包含用户活动身份、请求元数据或凭据。`instance_id` 在每个运行时实例新建时变化，`snapshot_seq` 在该实例内随配置、接受任务、进度、诊断或提交发布递增，区别于持久化状态 revision 和配置文件 revision。`config.enabled` 是宿主开关约束后的生效值，`summary.business_enabled` 保留业务开关，草稿仍来自 `/config-editor`。旧的 summary/accounts/config/capabilities 接口保留兼容用途，不能跨接口假定同一版本。

额度查询和手动唤醒的 202 响应包含 `job_id`；内容生成的 `job_id` 等于已有 `request_id`。接受的任务在返回前可从 `summary.scheduler.jobs` 读取；该数组保留最近 64 个任务，元素包含 `job_id/kind/phase/account_id/updated_at` 及 `quota_query`、`manual_wake` 或 `prompt_generation` 详细结果。清单/能力暂时不可用时带 `blocked_reason/retry_at`，任务开始后达到 2 分钟仍被阻塞时，在调度检查中失败结束。已发送操作的验证和安全抑制不受该未发送等待期限控制；送达未知为 unknown，不自动重发。任务记录不跨重启恢复，已持久化的模型派发保护继续有效。

面板按 job_id 读取对应结果，收到其他账号的新任务也不替换本地跟踪的任务；实例变化或记录淘汰会明确显示。所有运行数据统一应用，使用读取序号、写操作版本与快照版本阻止旧响应覆盖，读取失败保留已知结果与未保存草稿。

## 观测回调和生命周期

`request.intercept_before`、`request.intercept_after`、`request.complete` 提交宿主认证活动事件，并维护有界的请求账号绑定。前两个回调原样回传 Headers 与 Body，不拒绝请求、不改写请求。`response.intercept_after`、`response.intercept_stream_chunk`、`websocket.response_event` 只解析绑定账号的额度并返回空修改对象，不改变任何响应内容。插件自身流量由 CPA 适配器排除；不可信或缺失的可选身份字段不会变成人数。队列由同一运行时协调器处理；详见 [额度适配器](cpa-adapter.md#正常请求返回的额度)。

初始化复制完整 host API 结构，不保存宿主传入的临时结构指针。每次 host callback 同步读取完整响应、验证长度与 Envelope，再按宿主提供的 free_buffer 释放；失败响应的已分配缓冲区也释放。入口验证 ABI、空指针及 64 MiB 缓冲上限；RPC 和后台运行的 Go panic 归类为错误，不能穿过 C ABI。

`plugin.quiesce` / `plugin.shutdown` 的 RPC 先停止收集活动、取消运行时，并最多等待五秒。若宿主 callback 尚未返回，RPC 返回未完成错误并保留状态锁，不报告关闭成功。**原生 void shutdown 返回前始终等待运行时 callback 结束**，随后关闭存储；宿主才能释放 callback context 并卸载动态库。

本地 Context 无法直接中断已经进入的 C host callback。宿主 callback 的 HTTP/模型超时与取消协议负责实际请求终止；若宿主永久不返回，原生 shutdown 会继续等待，不能同时承诺有界卸载和未返回 callback 的安全性。此实现不做额外 self-dlopen/pin，也不声称 Go c-shared 可在任意宿主中安全反复热卸载；推荐通过进程重启更换二进制。

当前协调器按串行方式执行网络操作，实际生成并发为 1（不会超过配置上限）；管理摘要明确报告 `effective_concurrency`。真实前台生成的原子防护能力以 `/capabilities` 为准。

## 本地验证

`internal/plugin` mock 测试覆盖注册、严格包装配置、原样拦截器、账号活动解析、管理认证路径隔离、可选调度汇总及活动身份不外泄、重新读取配置与通知，以及等待 callback 完成前不关闭存储。

`cmd/plugin/testdata/fake_host.c` 仅用测试回调，通过 dlopen 验证 ABI/schema 握手、临时 API 结构被覆盖后的正确调用、方法与长度守卫、缓冲区释放和 quiesce/shutdown。测试策略关闭生成，fake host 不发送真实请求，也不连接外网。构建示例：

```sh
cc -Wall -Wextra -Werror -o /tmp/codex-window-refresh-fake-host cmd/plugin/testdata/fake_host.c -ldl
/tmp/codex-window-refresh-fake-host /tmp/codex-window-refresh.so /tmp/codex-window-refresh-lifecycle.json
```

生命周期 JSON 需含 schema_version 6 和 base64 编码的包装 YAML 字节；模拟真实 CPA 的包装 YAML 还应含 `enabled: true`、`priority: 0`（官方字段 `config_yaml` 是 `[]byte`）。fake host 在进程退出时释放映射，不模拟生产宿主的热卸载。

原生结构声明参考官方 MIT 示例，来源许可保存在 `cmd/plugin/NOTICE`；规则、服务和运行时实现为本项目代码。

`summary.scheduler.plan.next_wakes` 按账号 ID 返回各账号最早的下一次唤醒安排，包含时间或预计范围。该摘要不依赖 `planning.enabled`，不受详细 `entries` 的 256 条上限影响；不包含查询任务、暂不可执行的时间或尚未分配账号的每日池任务。它只读取现有规则和状态，不创建意图或发送请求，未知时间由面板显示原因。
