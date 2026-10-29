# 项目账号调度与官方重置卡

V2 预发布版本 v0.2.0-rc.21 增加业务选号和赠送重置卡管理。它们分别有开关，升级默认关闭；旧查询、唤醒规则保留。需要同时更新原生桥和独立后台并重启 CPA，数据库迁移到 schema 10。升级前备份一致数据库；不能把 schema 10 直接交给 rc.20 或更早后台。

## 项目与账号

在面板“账号调度与重置卡”填写项目首选账号，每行例如：

```text
project-a = account-1, account-2
project-b = account-3
```

这些是软偏好：首选账号不可用、额度证据过期或达到并发上限时，可以回退其他可用候选。项目标识不授予账号访问权限。CPA 原有模型支持、禁用、冷却、显式指定账号和账号优先级继续生效；本插件默认不跨 CPA 优先级层选择。已有其他选号插件时，应避免同时启用多个选号插件，宿主只选择一个活动 Scheduler。

客户端使用仓库或安装包内的 Python 启动器：

```sh
python3 scripts/cwr-codex.py --provider cpa --project project-a --directory /work/project-a
```

`--provider` 必须是用户级 Codex 配置中已经存在、指向 CPA 的 provider ID。启动器通过命令行设置该 provider 和 `X-CWR-Project` 请求头，不保存密钥，也不修改 Codex 配置文件。当前 Codex 忽略项目 `.codex/config.toml` 中的 provider 配置，不能通过各目录的 provider 配置实现切换。

自动按目录选择时，创建 `~/.config/cwr/projects.json`：

```json
{"projects":[
  {"directory":"/work/project-a","project":"project-a"},
  {"directory":"/work/project-b","project":"project-b"}
]}
```

然后在对应目录执行 `python3 /path/to/cwr-codex.py --provider cpa`。映射包含子目录，使用解析符号链接后的最长目录匹配；可以用 `--map` 指定另一份映射。Codex 参数放在 `--` 后，例如 `-- exec "检查测试结果"`。目录统一由启动器的 `--directory` 指定。

## 额度分配

选择顺序为：宿主可用候选 → 项目首选组 → 有效会话绑定 → 临近重置/重置卡到期 → 当前并发占用。会话绑定按已认证 caller scope、会话 ID 和项目共同隔离；无法取得完整身份时不建立绑定。

只有可信且新鲜的五小时、周额度及 permission 证据进入选号快照；已耗尽、过期或身份替换的额度不获得优先。原生桥只读取内存，每次选号不访问数据库、不主动查询上游。后台每两秒更新快照，快照十秒未续期就停止参与选号并交回宿主。额度证据默认最长有效五分钟，仍需开启合适的自动额度查询以持续获得优先调度。

并发保护包含选号后短暂预约、账号选定后的活动请求以及完成释放。短暂预约十五秒过期，缺失完成的活动记录最长保留十分钟；它是基于生命周期观测的本地保护，不提供跨进程、跨插件的原子执行租约。没有任何可用可信快照候选时交回内置选择器；已有可信候选全部忙碌时返回 `scheduler_busy`。

## 官方赠送重置卡

后台需要可执行的 Codex CLI，且必须支持官方文档中的 `account/rateLimits/read`、`rateLimitResetCredit/consume` 和外部 ChatGPT token 登录。可通过后台参数 `--codex-binary /absolute/path/to/codex` 指定。旧版、不支持该协议或未安装时展示诊断，不调用网页内部端点。

每次账号操作使用独立临时 CODEX_HOME、内存凭据存储和 stdio RPC，凭据仅在内存及匿名管道传输，不作为命令参数、日志或数据库内容。适配器不开模型会话。账号显式代理/direct 设置会被继承；账号没有代理覆盖时，重置卡适配器使用 `--reset-proxy`，未指定则继承后台进程代理环境。它使用 Codex 进程网络栈，**不会自动读取 CPA 全局代理**；需要代理的部署应为该后台单独配置。既有额度查询仍通过原有 CPA HTTP 桥继承宿主全局代理。

面板分别开启“后台查询重置卡”和“到期前自动使用”。默认每账号十五分钟查询一次，接近已知到期时间时提前补读；所有卡片请求串行执行，大账号池实际轮询周期还受账号数和响应耗时影响。服务缺少到期时间或仅返回卡片数量时，保留未知状态，不猜测日期、不自动兑换。

默认关注一天内到期的卡，优先最早到期的一张；五小时或周窗口已用达到 80% 时可以兑换，最后三十分钟内则只要存在有用的已使用窗口就尝试。自然重置距离不足五分钟的窗口不作为兑换理由。执行前先查询额度、核实账号绑定、检查配置和其他后台发送占用。不能重置的窗口由官方返回 `nothingToReset`，不会创建模型请求来人为消耗额度。

兑换前持久保存操作 ID，并作为 `idempotencyKey`；超时、响应丢失、重启后复用同一 ID 和同一张卡。明确返回 `reset` 或 `alreadyRedeemed` 后只做额度和卡片补读，不再次兑换；读到五小时和周额度均可用后，清除 CPA 对该账号的本地路由冷却并请求重新同步；已因额度耗尽而暂不可用的账号仍可核实和兑换，手动禁用始终阻止兑换。完成后显示“已兑换并查询额度”，不根据兑换回执构造新的倒计时。`noCredit`、`nothingToReset` 记录为本次未兑换。结果未知保留账号后台发送保护；关闭自动兑换后不会继续变更操作，已确认兑换的只读核实可以继续。

管理 API：`GET/POST /v0/management/codex-window-refresh/v2/scheduling` 使用 revision 比较保存；`GET .../reset-cards` 读取卡片与最近兑换记录。它们通过 CPA 管理认证，读取设置和打开页面不会查询上游或兑换。

接口依据：[CPA Scheduler](https://github.com/router-for-me/CLIProxyAPI/blob/90c447d3a8727d115aa7552ec93ce023736ed492/sdk/pluginapi/types.go#L520)、[Codex 配置](https://learn.chatgpt.com/docs/config-file/config-reference)、[官方 App Server 账号协议](https://learn.chatgpt.com/docs/app-server#8-earned-rate-limit-resets-chatgpt)。本地验证使用模拟宿主和模拟协议进程，未操作真实重置卡。
