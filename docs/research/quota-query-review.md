# “查询此账号额度”故障检查与方案对比

适用范围：V1 额度查询故障分析。当前使用入口见 [文档导航](../README.md)。

核对日期：2026-10-13。用户报告更新了 CPA；本次未连接用户服务器，未获取真实账号凭据或请求真实上游。以下区分确定的代码缺陷、方案限制和部署后仍需确认的事项。

后续完善已在本地源代码完成：操作名修正；查询失败分阶段分类并发布脱敏 HTTP 状态和耗时；新增显式宿主操作创建/取消检查，查看状态不自动探测；支持四种代理字段并拒绝冲突；原生和管理桥统一使用官方额度面板请求头；管理桥兼容 status_code/statusCode；面板提供单账号查询超时，插件 transport 上限与配置最大值一致。下文“其余问题”保留修复前检查记录，其中错误分类、接口检查、代理别名、请求头和插件超时限制已经完善。物理凭据滞后、非标准嵌套凭据及真实 CPA 联调仍是边界，不宣称已解决。

完善后的验证：全量 Go 测试、go vet、全量 race 检测、20 项前端逻辑测试通过。原生 C 假宿主完成自动与单账号额度查询两次及显式连接检查，模型调用为 0，22 个宿主响应缓冲区全部释放。真实 Chromium 浏览器验证通过，覆盖接口检查、账号卡片的失败阶段/HTTP 状态/耗时、查询超时保存、查询与模型唤醒分离及五个移动端页面。所有数据均来自本地模拟宿主或 loopback 测试服务，没有查询用户真实账号。

## 已确认的直接缺陷

默认 `quota_transport: host` 在账号没有专用代理时，先创建宿主 HTTP 操作，再调用额度接口。原代码使用：

```text
host.http.operation.open
```

CPA 官方实际方法名为：

```text
host.http.operation_open
```

来源：[固定官方提交的 sdk/pluginabi/types.go](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginabi/types.go#L97)。[宿主回调分发](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_callbacks.go#L160)只接受该常量；其他方法进入 unsupported host callback 分支。

同时重新读取了[官方当前 main 的 ABI 定义](https://github.com/router-for-me/CLIProxyAPI/blob/main/sdk/pluginabi/types.go)，仍为 operation_open；该结论并非只依据旧的固定源码快照。

因此这条路径会在 `host.http.do` 之前失败，还没有发出 wham/usage 网络请求。`NativeReader.hostHTTP` 把操作创建错误归为 `quota_host_http_failed`，页面再显示“CPA HTTP 查询失败，请检查连接或使用兼容管理桥”。这解释了报错，但提示把接口调用错误混在网络故障中。

影响范围：无账号专用代理的宿主查询，包括只配置 CPA 全局代理的账号；批量、自动和单账号查询共用该 reader。配置了被当前解析器识别的账号代理时，走插件私有 HTTP transport，不经过错误方法名。管理桥模式也不经过此回调。升级 CPA 不会改正旧插件发送的方法名，修复需要更新插件动态库。

原 Go 测试、C 假宿主和适配器文档同时使用了错误名称，构成了实现和测试相互印证错误的情况。本次已修正生产调用、测试、C 假宿主及文档。测试使用官方方法名的独立字面量，不从生产代码读取方法名。

## 对比代表性社区方案

选择官方管理中心、CPA-Manager-Plus 及同类原生唤醒插件。前两者是额度管理工具，并非原生动态库插件；第三者是真正的原生插件，但它不提供额度查询，不能直接拿它验证额度接口。

| 方案 | 查询或执行链路 | 对当前方案的启示 |
| --- | --- | --- |
| 官方 Cli-Proxy-API-Management-Center | 额度查询通过管理 `api-call`，传 authIndex、固定 wham/usage URL、Bearer $TOKEN$ 和账号 header；另外设置 User-Agent | 由 CPA 解析运行时 token、选择账号及全局代理；浏览器使用已登录管理凭据 |
| CPA-Manager-Plus | 后台 Codex 检查同样 POST `/v0/management/api-call`；独立处理管理 HTTP 状态、上游 status_code 和超时；请求带 User-Agent | 网络、认证和上游状态应分层诊断；后台长期任务仍需后台可用的管理凭据 |
| elunez/codex-keepalive | 通过 host.auth.get 读取访问凭据；有账号代理时自行 HTTP，否则 host.Do；发送的是模型唤醒请求，不读取额度 | 直接宿主模式有同类实现依据；凭据字段适配和宿主 RPC 契约必须单独核对，不能复制模型请求作为额度查询 |
| 当前插件 host 模式 | 读取物理凭据 JSON；无账号代理使用 host HTTP，有账号代理使用私有 transport | 无需后台管理密钥，但承担文件凭据滞后、字段兼容和 RPC 方法维护成本 |
| 当前插件 management 模式 | 管理 api-call + $TOKEN$ + auth_index | 与官方管理中心、CPAMP 的额度路径一致，但后台必须配置 management_url 和密钥环境变量 |

源码依据：

- [官方额度请求](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/main/src/features/quota/providers/codex/data.ts#L418)、[请求头](https://github.com/router-for-me/Cli-Proxy-API-Management-Center/blob/main/src/utils/quota/constants.ts#L141)。
- [CPAMP 后台探测](https://github.com/seakee/CPA-Manager-Plus/blob/dbf61ddf438f399558722df3467355c42af9005d/apps/manager-server/internal/service/codexinspection/service.go#L1865)。
- [keepalive 原生执行与凭据解析](https://github.com/elunez/codex-keepalive/blob/main/activation_direct.go#L92)、[项目功能边界](https://github.com/elunez/codex-keepalive)。

## 当前方案的其余问题

### 1. 错误分类不足，难以定位故障阶段

`internal/cpa/native_quota.go` 将操作创建失败、HTTP 回调失败和上下文超时统一报告为 `quota_host_http_failed`。`cmd/plugin/main_linux.go` 与 `cmd/plugin/response.go` 还会将非零 ABI 返回码或 RPC 拒绝简化为固定错误。管理桥把管理认证失败、管理路由不存在、上游 401/403/429/5xx、响应不合法等统一归为 `quota_management_bridge_failed`，页面没有针对该分类的专门说明。

建议下一步记录安全的结构化诊断：阶段（operation_open / http_do / management_http / upstream_http / parse）、固定错误类别、HTTP 状态及耗时。不要直接保存宿主错误字符串、响应正文、token 或代理 URL。优先让“接口不存在”和“超时”与网络失败分开。

### 2. “查询就绪”只验证对象和配置存在

`NativeReader.Available` 只判断 callback 非空，Host 的 QuotaAdapterReady 只据此计算。此处不验证方法是否可用，更不测试每个账号的上游连通性。当前页面文字已说明这只是配置检查，但它仍无法提前发现此次方法拼写问题。

建议将配置可用、宿主操作接口验证、最近一次查询结果分别显示。若主动探测 operation_open，必须立即用 cancel 清理句柄，避免留下操作资源；也不能因为一个账号查询失败就标记全部账号无法查询。

### 3. 文件凭据路径弱于宿主运行时 token 路径

官方 [host.auth.get](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/auth_callbacks.go#L245) 读取物理文件；管理 [tokenValueForAuth / resolveTokenForAuth](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/api_tools.go#L254) 读取运行时元数据。当前原生模式还会在文件 expired 已过期时直接拒绝查询。

文件落后于内存可能使原生模式失败而管理中心成功。但在核对的官方提交中，resolveTokenForAuth 对 Codex 直接返回现有 token，并没有像其他 provider 那样按需刷新。因此管理桥有利于复用运行时 token，不等于能保证刷新过期的 Codex OAuth。当前两种模式的账号身份核对仍依赖物理文件，管理桥也不能解决所有 runtime-only 或非标准凭据问题。

### 4. 凭据与代理字段兼容范围较窄

当前宿主模式读取顶层 type/account_id/access_token/token/expired，代理只支持 proxy_url/proxyUrl。同类 keepalive 还接受 proxy-url/proxyURL 及部分嵌套凭据。若目标 fork 使用未支持的代理字段，当前插件会按“没有账号代理”处理，转而使用全局代理。

建议仅根据目标 CPA 实际输出补充有证据的字段，并检查重复字段冲突；不要把同类代码的所有 token 候选直接照搬，尤其不要把 id_token 当成访问 token。此项是兼容范围，不是已确认的用户账号问题。

### 5. 请求头与主流额度查询不完全一致

当前原生和管理桥额度请求设置 Authorization、Chatgpt-Account-Id、Accept，没有设置 User-Agent。官方管理中心和 CPAMP 都设置 Codex User-Agent，CPAMP 后台允许配置。

这应纳入实际请求对比，但不能认定缺失 User-Agent 一定导致上游拒绝，更不能把它当成本次错误方法名的解释：原错误路径尚未发出上游请求。若修复后仍发生 403/非 JSON 响应，再核对实际 HTTP 状态和请求头。

### 6. 对宿主和传输的验证仍不足

插件只检查 ABI 1 / schema 6；[schema 6 的定义](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginabi/types.go#L17)是管理 JSON 响应语义，不是额度查询接口的能力声明。当前默认查询超时为 15 秒，代理网络较慢时也会进入统一错误；私有代理 transport 另有 30 秒超时，但实际受外层更短 deadline 限制。

本地假宿主验证能发现 ABI、方法名和资源释放问题，不能验证真实网络、OAuth 文件与内存一致性、账号代理或上游请求头。正式发布前仍需在真实 CPA 上做一次查询联调。

## 修复验证与部署建议

本次验证：

1. 原实现与原测试全部通过。
2. 仅把测试宿主改为接受官方 operation_open 后，额度读取、401/403 分类和取消测试失败；取消测试显示 HTTP 请求根本没有启动。
3. 修正生产方法名后，`make check native-smoke` 通过：全量 Go 测试、go vet、动态库构建及原生 ABI 验证。C 假宿主完成自动及单账号额度查询 2 次，模型调用 0 次，20 个宿主响应缓冲区全部释放。

已生成本地 `bin/codex-window-refresh.so`。本次未发布新版、未更新用户服务器或重启服务；安装器下载的旧发布包仍可能含原错误，不能仅靠重复安装相同 release 获得修复。

建议先部署含此修正的插件，保留默认 host 模式，再与原 CPA 管理中心对同一账号的查询结果比较。若原面板成功而插件仍失败，再优先查文件凭据、代理字段和请求头差异；已有安全配置的管理桥可作为兼容路径。无需为了这个拼写错误永久切换默认方案或引入模型请求。
