# 配置与管理接口设计

状态：配置解析、网页规则编辑和带内容版本检查的原子保存已实现；本文件的分页、独立验证及幂等变更接口仍为扩展设计契约。当前实际注册的接口见 [插件说明](plugin.md)，其路径与下面拟议完整版不同；面板使用见 [网页面板](panel.md)。

本文件落实 [调度逻辑设计](codex-refresh-design.md)。可复制配置见 [config.example.yaml](../config.example.yaml)。接口通过宿主管理入口认证，不能公开给普通代理 API key；当前管理认证与资源路由已按固定官方宿主接口核对。

## 1. 配置契约

配置采用 YAML，解析后按 JSON 数据类型验证。禁止未知字段、重复 YAML 键和重复规则 ID，禁止 NaN、Infinity 及隐式转换字符串为布尔或数字。必须完整验证后原子应用，不接受部分成功。

时间统一持久化为 UTC RFC 3339 时间戳；配置里的日历时间使用 `timezone`。持续时间采用整数和单位组成的字符串，例如 `0s`、`30s`、`10m`、`2h`，单位为 `s/m/h/d`，不允许负数、小数或混合单位；内部换算为整数毫秒并检查溢出。每日时间精度为分钟；任务实际随机时间精度为秒。

下表中的范围属于第一版可验证的配置约束，不代表上游额度保证。

### 1.1 池配置

| 字段 | 类型、默认值 | 范围与语义 |
| --- | --- | --- |
| `schema_version` | integer，`1` | 只接受 `1`；未知版本拒绝加载 |
| `enabled` | boolean，`false` | 自动模型唤醒总开关；需用户手动开启并保存，关闭后不创建或发送自动任务，已有操作继续验证；明确确认的单账号手动唤醒独立于此开关，额度查询和被动采集独立控制 |
| `quota_query.automatic` | boolean，`false` | 普通定期额度查询默认关闭，需用户手动开启并保存；不发送模型请求，关闭仍保留被动额度、独立启用的唤醒前核对和已有操作验证 |
| `quota_query.on_five_hour_expiry` | boolean，`false` | 已确认的五小时窗口到期后查询一次，独立于普通定期查询和模型唤醒；失败或缺少窗口字段按间隔及退避重试；成功边界标记持久化 |
| `quota_query.on_weekly_expiry` | boolean，`false` | 已确认的周窗口到期后查询一次，规则同上；两种窗口同时到期合并为一次账号额度查询 |
| `planning.enabled` | boolean，`false` | 自动生成未来查询及唤醒预览；只读计算，不开启或增加请求，不重新抽取已分配的随机时间 |
| `planning.horizon_days` | integer，`7` | 1–30 天；最多显示最早 256 项，周期查询以循环条目表示，窗口未知时不推算未来周期 |
| `quota_query.manual` | boolean，`true` | 仅为兼容旧配置保留，解析时归一化为 true；手动查询始终允许，false 不再拒绝或取消查询 |
| `quota_query.interval` | duration，`30s` | 普通定期查询间隔，允许 30 秒到 1 小时；完整被动额度也按此间隔推迟重复轮询 |
| `quota_query.five_hour/weekly.periodic_enabled` | boolean，`true` | 本类额度是否参与定期查询；仍需 `quota_query.automatic` 总开关开启。两类均关闭则不定期查询，默认总开关仍关闭 |
| `quota_query.five_hour/weekly.interval` | duration，`0s` | `0s` 继承公共间隔，否则为 30s–1h 且不得短于公共间隔；两类查询独立调度，同账号接口请求会合并 |
| `quota_query.five_hour/weekly.expiry.excluded_hours` | string array，`[]` | 到期自动查询的每日排除时段；按 `timezone`，支持跨午夜、不重叠，全天排除写 `00:00-24:00`，留空不排除 |
| `quota_query.five_hour/weekly.periodic.excluded_hours` | string array，`[]` | 定期查询独立排除时段，与到期查询互不影响；延迟到期仍处于排除时段时顺延至结束 |
| `quota_query.five_hour/weekly.expiry.random_delay.min/max` | duration，`0s/0s` | 到期边界后随机延迟，`0 ≤ min ≤ max ≤ 24h`；按账号身份、凭据代际、窗口与边界生成稳定随机值，预览和重启不会重新抽取同一到期边界 |
| `quota_query.five_hour/weekly.periodic.random_delay.min/max` | duration，`0s/0s` | 每次定期查询在间隔后附加的随机延迟；初次从引擎启动时间计算。下一次以最近查询或完整被动观测为基准，重复读取计划不重新抽取 |
| `timezone` | string，`Asia/Shanghai` | 可加载的 IANA 时区；拒绝未识别名称，不能默默退回服务器时区 |
| `reset_rules.five_hour.enabled` | boolean，`true` | 创建普通到期任务；关闭不影响周联合任务和每日目标选取到期账号 |
| `reset_rules.five_hour.random_delay.min/max` | duration，`0s/10m` | `0 ≤ min ≤ max ≤ 24h`；事件到期后只抽一次 |
| `reset_rules.five_hour.model/prompt` | string，空字符串 | 五小时到期和每日时间规则专用模型/消息，分别留空时继承 execution 默认值；模型 ≤ 256 字节，消息 ≤ 4096 字节 |
| `reset_rules.weekly.enabled` | boolean，`true` | 创建周联合重置任务 |
| `reset_rules.weekly.random_delay.min/max` | duration，`0s/10m` | `0 ≤ min ≤ max ≤ 24h` |
| `reset_rules.weekly.model/prompt` | string，空字符串 | 周到期唤醒专用模型/消息，分别留空时继承 execution 默认值；与五小时设置互不影响 |
| `ordinary_rules.daily_enabled` | boolean，`true` | 每日时间点 / 随机时段规则独立开关；与五小时到期唤醒独立，仍需自动模型唤醒总开关开启。旧策略继承 true，不改变原有每日规则；关闭不删除规则 |
| `reset_rules.weekly.bypass_activity_limit` | boolean，`true` | 当前必须为 `true`；周任务不受人数比例和普通最大账号数限制 |
| `reset_rules.weekly.bypass_allowed_hours` | boolean，`true` | 当前必须为 `true`；周任务夜间也执行 |
| `activity.lookback` | duration，`30m` | `1m～24h`；真实用户最近活动统计窗口，长请求期间仍活跃 |
| `activity.accounts_per_user` | finite number，`1.0` | `0～100`；向上取整 `U × 比例`。0 关闭人数驱动容量需求，不关闭周任务或每日最低目标 |
| `activity.max_accounts` | integer，`10` | `0～100000`；仅限普通规则目标。0 禁用普通容量补充；周任务仍可执行 |
| `activity.identity_source` | enum，`host_caller_scope` | 可选 `host_caller_scope`、`host_user`、`mapped_user`、`api_key_hash`，具体口径见第 3 节 |
| `ordinary_rules.allowed_hours` | string[]，`["08:00-23:00"]` | 日历时间段，见第 1.3 节；空数组表示无普通允许时段 |
| `ordinary_rules.allow_outside_hours_when_users_active` | boolean，`true` | 允许时段外，只有 `U > 0` 的人数任务可以继续执行；仍受普通目标约束 |
| `daily_rules` | DailyRule[]，示例晨间规则 | 未配置默认空数组，示例文件显式提供晨间规则；最多 100 条 |
| `execution.model` | string，无默认值 | 启用生成时必填；非空、长度 ≤ 256；示例占位符拒绝应用。模型支持性依赖宿主能力核对 |
| `execution.prompt` | string，`Reply OK.` | 非空、UTF-8 字节数 ≤ 4096；禁止记录请求正文 |
| `execution.max_concurrency` | integer，`1` | `1～32`；限制所有唤醒生成，包括周任务 |
| `execution.quota_read_timeout` | duration，`15s` | `1s～5m`；额度与账号清单查询的 Context 超时；不能强行中断宿主 C 调用 |
| `execution.request_timeout` | duration，`1m` | `1s～30m`；后台模型请求的 Context 超时；超时保留未知送达抑制 |
| `execution.min_dispatch_spacing` | duration，`5s` | `0s～1h`；全局连续生成开始时间的最小间隔，包括周任务 |
| `execution.quota_max_age_before_send` | duration，`30s` | `0s～5m`；0 表示每次生成前主动查询；适配层必须提供“对本次操作已完成主动查询”的证明，不能把 0 转成正数缓存年龄替代原语义；缓存可信性不只由年龄决定 |
| `execution.verify_delays` | duration[]，`[5s,30s,2m]` | 1～10 项，严格递增且每项 `1s～1h`；均相对发送结束/不确定结果发生时间，后续证据等待继续低频观测 |
| `execution.verification_capacity_hold` | duration，`15m` | `1m～24h`，不得短于最大验证延迟；从发送完成/转不确定时起保留容量预约，到期可释放 P，但不解除任务重复发送抑制 |
| `execution.catch_up` | boolean，`true` | 当前必须为 `true`；不允许关闭用户已确定的补发行为 |
| `account_overrides` | map<string,AccountOverride>，`{}` | 键为宿主稳定账号 ID；不得携带凭据；最多 100000 个 |

`max_accounts` 是普通准备容量上限，`max_concurrency` 是同时生成数量，`min_dispatch_spacing` 是请求开始的间隔。三者不能互相替代。周任务绕过前者，仍受后两者控制，也必须通过账号有效性、窗口、额度和结果不确定检查。

`quota_max_age_before_send=0` 在运行配置中拆成强制本轮主动查询和正数证据新鲜度阈值（首版内部 30 秒）。查询必须匹配本次账号代际和新查询序号，不能把零仅换成 30 秒后复用缓存；各额度字段独立检查新鲜度。已有 `CapacityHoldUntil` 不因修改 `verification_capacity_hold` 重新起算，新值只用于新操作。

### 1.2 每日规则与账号覆盖

DailyRule：

| 字段 | 类型、默认值 | 范围与语义 |
| --- | --- | --- |
| `id` | string，必填 | 全池唯一，正则 `^[a-zA-Z0-9_-]{1,64}$`；改 ID 等同删除旧规则并添加新规则 |
| `enabled` | boolean，`true` | 关闭后暂停该规则的未执行目标 |
| `random_time_range` | string，必填 | `HH:mm-HH:mm`；第一版不允许跨午夜，结束可写 `24:00`；开始与结束相同表示精确时间，随机抖动为 0 |
| `minimum_ready_accounts` | integer，`1` | `0～activity.max_accounts`；0 表示该规则不增加容量目标；不得超过已知的普通参与账号数，否则报容量警告 |
| `catch_up` | boolean，`true` | 当前必须为 `true`；恢复时只保留最近有效事件 |

每日事件先形成池级容量目标，再选取所需到期账号。其目标与人数目标取最大值，同日重叠规则的最低目标也取最大值，不相加；规则事件、随机时间仍各自持久化，不会提前满足尚未到时的规则。已有可用账号满足最低目标时直接完成目标，无须发送。

AccountOverride 只接受下列可选字段。字段省略则继承；显式 `null` 通过 API 清除覆盖后移除该键，落盘 YAML 不保留 `null`。

| 字段 | 类型 | 作用 |
| --- | --- | --- |
| `paused` | boolean | 默认为 false；暂停该账号所有新生成，包括周任务，已发送验证继续 |
| `five_hour_enabled` | boolean | 允许或退出普通候选池（人数及每日目标）；关闭后不计入普通 `N/A/P`，不影响周任务 |
| `weekly_enabled` | boolean | 是否参加周规则，不改变普通参与资格 |
| `five_hour_random_delay` | `{min:duration,max:duration}` | 覆盖该账号未来的短期到期事件随机范围，范围同池配置 |
| `weekly_random_delay` | `{min:duration,max:duration}` | 覆盖该账号未来的周事件随机范围 |
| `allowed_hours` | string[] | 覆盖普通时段；不影响周任务 |
| `allow_outside_hours_when_users_active` | boolean | 覆盖普通夜间活动开关；不影响周任务 |

池级规则关闭是总开关：账号覆盖不能重新启用池级已关闭的周规则或普通到期事件。`five_hour_enabled` 的账号级有效参与值同时决定人数和每日选取范围，适配为核心 `OrdinaryParticipant`；池级 `reset_rules.five_hour.enabled` 只控制自动短期到期事件，不关闭独立每日目标，避免误将每日规则理解成可使用退出普通池的账号。

账号级不提供时区、人数比例、普通最大数量、并发、发送间隔覆盖，防止多个账号重复计算池容量或绕过全局保护。配置中未找到的宿主账号 ID 作为休眠覆盖保留并给出警告，不自动绑定名称相似的新账号。

### 1.3 时段、边界和重叠

普通 `allowed_hours` 每项为 `HH:mm-HH:mm`，开始只允许 `00:00～23:59`，结束额外允许 `24:00`。区间为半开 `[start,end)`，23:00 结束的时段在 23:00 已属于时段外。`00:00-24:00` 表示全天；同起终点如 `08:00-08:00` 是空时段，拒绝配置，避免误当全天。

跨午夜允许，如 `22:00-02:00` 规范化为 `[22:00,24:00)` 与 `[00:00,02:00)`。所有片段展开后检查重叠；相邻片段允许并合并，重叠片段拒绝，如 `22:00-02:00` 与 `01:00-03:00`。空数组意味着普通任务仅可能按夜间近期活动例外执行；如果例外也关闭，普通自动任务不会发送，页面给出阻塞警告。

每日规则第一版采用同日范围，不跨夜；需要午夜两侧范围时使用两个显式 ID。每日规则彼此时间重叠合法，但给出 `DAILY_RULE_OVERLAP` 提示，解释容量取最大值、请求按账号合并。完全位于普通允许时段外的每日规则给出 `DAILY_OUTSIDE_ALLOWED_HOURS` 提示，按下一允许时段补发，不借用普通夜间人数例外提前准备。若允许时段为空，提示该每日目标无法执行。

时区出现夏令时跳跃时，不存在的每日时间移到当日第一个合法时间；重复时间选择较早 UTC 映射。随机范围先映射成实际 UTC 区间再抽样；映射后无合法范围则只触发一次边界时间，记录 `calendar_adjustment`。事件以规则 ID、配置代际与本地日期标识持久化，不能因时钟回拨再次创建同日事件。

## 2. 配置热更新

配置返回 `revision` 修订号及规范化文档。更新必须携带 `If-Match`，避免两个管理员相互覆盖。保存时先验证、显示影响，再原子持久化并发布同一版本的运行配置；落盘失败不切换运行版本。首次无有效配置时禁用生成，只开放管理与观测。

| 改动 | 已存在事件与操作的处理 |
| --- | --- |
| 调整随机延迟或每日时间范围 | 已创建事件保留抽中的时间；新事件使用新值，页面显示旧/新配置代际 |
| 调整人数比例、最大普通账号数或统计时长 | 立即重算 `U/T/A/P/K`，撤回超过目标且未发送的普通预约；到期意图保留 |
| 修改普通时段或夜间开关 | 立即重做准入，不重抽随机时间；到期任务保留补发 |
| 关闭规则、暂停账号或关闭插件 | 停止尚未发送的对应任务；保留原事件，恢复后按当前状态补发；已发送继续验证 |
| 删除每日规则 | 尚未执行的该规则目标标记 `superseded`，已发送事件仅继续验证，不回滚请求 |
| 更改时区 | 已抽取 UTC 时间保持不变；新日历事件使用新时区，从当前时间起建立新代际，不回放旧日期；同规则未完成旧代际目标与新目标合并，至多保留最近有效目标 |
| 改变模型或 prompt | 仅影响未发送的未来请求；发送操作记录模型标识与配置版本，不存正文 |
| 更改账号凭据 | 不属于配置覆盖；宿主事件触发代际重建，旧任务不能转移给新凭据 |

管理端提供“立即重新检查”，只重新取得观测并重算现有准入条件，不提前越过随机到时点，不重抽时间、不生成新的手动唤醒任务。第一版不提供绕过有效窗口或未知结果抑制的“强制发送”功能，也不提供隐含的任务随机时间重新安排；将来显式重新安排必须新建事件版本且保留审计。

## 3. 用户统计口径

默认 `host_caller_scope` 使用宿主 `requestExecutionMetadata.metadata.caller_scope`：已核对的官方 v8 源码由认证 `session.CallerScope` 对 principal/key 生成 SHA256 标识。它可在请求开始时参与去重，并不证明独立自然人身份。只有宿主自己写入、来自已认证入口的 metadata 可以接入统计，不信任客户端同名字段。

同时使用宿主 `intercept_after` metadata 的 `selected_auth_id`/`selected_auth_index` 关联真实业务请求与目标账号，跟踪前台占用；结束或取消时释放。同一业务请求必须沿用稳定 `requestID` 完成 `start → afterselect → complete` 关联，宿主重试沿用同一个 ID。`start` 按请求 ID 只计一次在途活动；多次 `afterselect` 更新当前账号绑定，不重复增加 inflight；若重试切换账号，先释放旧绑定再绑定新账号；`complete` 只释放一次。缺少关联能力不能假装持续跟踪在途请求。具体源码证据与版本边界见 [CPA 宿主接口设计](cpa-integration.md)。

无 caller_scope 的旧版本仅在 usage 完成事件提供可信身份且确认来自真实业务入口时可以明确降级为 completion-only；把 usage 身份本地不可逆化后统计，页面显示“仅完成请求，无法准确观测当前在途用户”。无法归因的 usage 仅更新可信额度信息，不计用户、不标记真实活动；`UsageRecord.Source` 不是插件内部请求标志，不能单凭它排除内部生成。若 completion-only 也没有可信身份或内部请求排除能力，人数统计不可用。启动能力检测决定全池统计口径，不能逐请求混用开始事件与完成事件的两个身份域。切换身份口径时建立新统计代际，不把同一用户旧、新标识相加。宿主提供用户映射时可以使用 `mapped_user`，必须是受管理权限保护的映射。

`host_user` 和 `mapped_user` 是严格模式，能力缺失则配置验证失败，不悄悄降级；`api_key_hash` 需要可信认证层给出的 key 身份。宿主 caller_scope 已为不可逆标识；插件对 usage/key 等原始身份使用安装实例保密盐进行不可逆化，禁止输出盐、原始 key、原始用户 ID。映射来源由宿主负责，第一版不另增保存原始 API key 的映射接口。

在完整请求生命周期可观测模式下，活动包括认证成功的正常业务请求；失败或重试仍按同一个身份去重，不因同一请求产生多个人。长请求在执行期间持续活跃，结束后按统计时长保留。completion-only 仅在完成时更新最近活动，不能保证及时覆盖长请求。插件自己的生成、额度查询、健康检查和管理操作均排除。

管理摘要同时显示 `configured_identity_source`、`effective_identity_source`、`statistics_generation`、`lookback`、`active_users`、`inflight_users`、`activity_observation_mode`、`degraded` 和具体原因。不可观测的 `inflight_users` 为 null，不是 0。caller_scope/key 口径下共享 principal/key 的多人只能算一个；同一用户多个 key 无映射时算多个，界面需在统计卡片旁说明。没有任何可验证身份来源时普通人数驱动任务等待 `identity_unavailable`，不能编造人数；周任务和每日最低目标可继续按自身规则判断。

## 4. 拟议管理 API

以下路径以 `/v0/management/plugins/codex-window-refresh` 为拟议前缀，尚未注册，须在确认宿主插件接口后实现。插件 ID 为 `codex-window-refresh`。列表返回稳定分页游标、观测时间和配置修订号，不分页返回全量凭据。时间均为 UTC；额度值包含单位与数据来源，未知字段返回 `null`，不能把未知显示为 0。

| 方法及相对路径 | 用途 |
| --- | --- |
| `GET /capabilities` | 宿主账号精确执行、额度查询、真实活动与用户身份能力；不可用项与原因 |
| `GET /config` | 脱敏的有效配置、覆盖、修订号与警告；响应 `ETag` |
| `POST /config/validate` | 验证完整候选配置，返回规范化配置、字段错误、警告、变更影响；不产生随机抽样或任务 |
| `PUT /config` | 带 `If-Match` 原子应用完整配置；行为同验证，返回新修订号 |
| `GET /summary` | 用户统计、`N/T/F/T_effective/A/P/K`、周队列数、全局生成并发及观察异常 |
| `GET /accounts` | 分页账号状态与有效覆盖；支持规则资格、暂停、等待原因筛选 |
| `GET /accounts/{account_id}` | 两个窗口、可信度、基线、合并事件、发送及验证状态 |
| `PATCH /accounts/{account_id}/overrides` | 带 `If-Match` 更新覆盖；只接受允许的覆盖字段；null 清除单字段 |
| `POST /accounts/{account_id}/recheck` | 幂等触发一次观测与准入重算；202 返回操作 ID，不保证立即生成 |
| `GET /jobs` | 分页未完成合并任务、意图、随机时间、覆盖事件和暂停原因 |
| `GET /operations/{operation_id}` | 发送结果、每窗口验证结果、下一验证时间；不包含 prompt 或凭据 |
| `GET /events` | 分页结构化审计日志，支持账号、规则、operation ID、原因和时间范围筛选 |

变更接口接受 `Idempotency-Key`（16～128 字符），同键及同规范化输入返回原结果；同键异输入返回 409。版本冲突不修改状态，返回当前修订号。路径账号 ID 先解码一次、验证宿主标识，再精确查找，不能用显示名称或子串匹配账号。

`POST /config/validate` 示例：

```json
{
  "base_revision": 12,
  "config": {
    "schema_version": 1,
    "activity": {"accounts_per_user": 0.5}
  }
}
```

以上 `config` 仅展示结构片段；实际提交完整配置，不能把缺失字段当作对当前配置的局部修改。管理前端先读取当前配置、修改、提交验证，再保存同一规范化对象。`base_revision` 只用于影响比较；`PUT` 的 `If-Match` 才是写入并发保护。

错误响应固定结构：

```json
{
  "error": {
    "code": "CONFIG_INVALID",
    "message": "配置验证失败",
    "fields": [
      {
        "path": "activity.accounts_per_user",
        "code": "NON_FINITE_NUMBER",
        "message": "比例必须是 0 到 100 之间的有限数值"
      }
    ]
  }
}
```

| HTTP / 代码 | 触发情况 |
| --- | --- |
| 400 `MALFORMED_CONFIG` | 无法解析 YAML/JSON、重复键、非支持内容类型 |
| 401/403 `MANAGEMENT_AUTH_REQUIRED` | 未通过宿主管理认证或无管理权限 |
| 404 `ACCOUNT_NOT_FOUND` | 操作对象不在当前宿主账号表；配置休眠覆盖的保存例外见第 1.2 节 |
| 409 `REVISION_CONFLICT` | 修订号过期或幂等键输入冲突 |
| 422 `CONFIG_INVALID` | 已解析但字段类型、范围或关联验证不通过 |
| 422 `HOST_CAPABILITY_MISSING` | 严格身份模式、账号精确执行等必要宿主能力缺失 |
| 503 `STORAGE_UNAVAILABLE` | 配置、审计或任务持久化失败；不能生成 |

字段验证代码至少覆盖 `UNKNOWN_FIELD`、`DUPLICATE_RULE_ID`、`INVALID_TIMEZONE`、`NON_FINITE_NUMBER`、`INVALID_DURATION`、`OUT_OF_RANGE`、`DELAY_RANGE_INVERTED`、`EMPTY_ALLOWED_INTERVAL`、`ALLOWED_INTERVAL_OVERLAP`、`DAILY_RANGE_CROSSES_MIDNIGHT`、`MINIMUM_EXCEEDS_MAX_ACCOUNTS`、`WEEKLY_EXEMPTION_REQUIRED`、`CATCH_UP_REQUIRED`、`MODEL_PLACEHOLDER`、`VERIFY_DELAYS_NOT_INCREASING`。代码稳定，错误文本可本地化。保存警告例如普通目标超过实际账号数、重叠每日规则、固定时间完全在允许时段外、用户统计降级，必须与会拒绝保存的字段错误区分。

## 5. 管理页面结构

### 总览

显示近期用户数及口径、人数比例、普通目标、固定最低目标、有效容量、预约容量、普通差额、周待处理数、正在发送数。并列显示“窗口已确认”和“发送完成但待确认”，防止用 HTTP 成功代替验证。

容量计算悬浮说明使用同一观测修订：`T = min(N,max_accounts,ceil(U×比例))`；`T_effective = min(N,max_accounts,max(T,F))`；`K = max(0,T_effective-A-P)`。A/P 是不重复的账号集合，周预约只在账号同时参与普通规则时计入 P。额外的周账号不受 `max_accounts` 限制，不误显示为普通超配。

### 规则设置

分别配置短期到期、周联合重置、普通允许时段及夜间活动、用户统计、每日容量、请求执行。周规则旁固定说明“夜间执行，不按人数限制；仍受有效窗口、并发和间隔检查”。随机设置提示“已创建任务不改变，未来任务使用新范围”。

每日规则编辑器预览时区、范围、最低目标、跨夜错误与允许时段重叠情况。保存前呈现规范化结果、修订冲突或字段错误，以及对已预约任务的影响；验证页面不创建任务。

### 账号与任务

账号表显示宿主稳定 ID 与显示名称、规则参与状态、5 小时/周额度和单位、重置时间、本地基线与观测来源、下一随机到时点、下一实际检查时间、触发原因、等待原因、配置代际、发送及验证结果。暂停和“重新检查”只提交相应管理操作，不能隐含发送。

展开任务显示覆盖事件列表，区分“原定时间”“随机时间”“因为时段/需求/故障等待的下一检查”。展示恢复补发的原因，不把等待伪装成倒计时已刷新。账户加入或退出普通池立刻重算容量，不能另建每账号人数配额。

### 日志与诊断

结构化事件至少包含 `event_id`、`at`、`account_id`、`rule_ids`、`cycle_ids`、`operation_id`、`config_revision`、`reason_code`、`decision`、`next_check_at`、`quota_source` 和容量快照。禁止持久化原始用户 ID、原始 key、token、prompt 或请求正文；只保存观测所需的额度字段，不直接倾倒响应头。

原因包括 `not_due`、`active_five_hour`、`quota_unknown`、`quota_exhausted`、`outside_hours`、`no_demand`、`target_satisfied`、`foreground_busy`、`backoff`、`credential_changed`、`weekly_reset_conflict`、`identity_unavailable`、`awaiting_evidence`。固定时间的规则满足与请求的窗口验证分别记录。

例：`账号 A 周任务已到随机时间；夜间/人数豁免；因 5 小时新窗口已确认不发送。` 例：`普通目标 2，已有 1，周预约 1，差额 0；账号 B 保留到期意图。`

## 6. 配置与页面验收场景

1. `22:00-02:00` 正常接受；与 `01:00-03:00` 同时配置报告展开后的重叠错误；相邻时段正常归一。
2. 比例为 `.nan`、字符串 `"0.5"`、负数或 101 均拒绝；比例 0 正常接受，周与每日目标保持各自规则。
3. 未知时区、未填写支持模型、周豁免为 false、补发为 false 均显示精确字段错误。
4. 两个晨间规则重叠发出提示，目标取最大值；不按每条规则、每个账号各发一遍。
5. 改延迟范围不修改持久化随机时间；改允许时段立即阻止尚未发送任务；改并发不终止已经发送请求。
6. 周任务在夜间 U=0 仍可准入；多个周账号遵守统一并发和发送间隔，不能借人数豁免同时全部生成。
7. 普通账号退出池后从 N/A/P 移除；周-only 账号可以继续周任务，但不满足普通容量目标。
8. 请求成功但只有短期证据时显示周等待验证；重新检查不产生重复请求；有效短期窗口始终不发送。
9. 两位管理员以同一旧 revision 保存，只允许第一位成功，第二位返回冲突并保留编辑草稿。
10. 身份从 key 统计切换用户统计时不重复计算历史身份；无法观测身份时显示人数不可用，不伪造 U=0；周与每日任务仍独立判断。
