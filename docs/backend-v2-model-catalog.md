# V2 账号模型目录：接口依据与边界

## 官方来源

本说明核对 CPA 官方仓库 `router-for-me/CLIProxyAPI` 的固定提交 `3de4e248f6bb12fc7dfafb183404f3b248578922`，不把源码中的能力视为本机部署已经支持。

- [管理路由注册](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/server_management.go#L180)：`GET /v0/management/auth-files` 和 `GET /v0/management/auth-files/models`。
- [GetAuthFileModels 实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/auth_files.go#L332)：必填查询参数 `name`，匹配运行时账号的 `FileName` 或 `ID`，再以账号 ID 读取模型注册表。响应为 `{"models":[{"id":"…"}]}`，可能附带 `display_name`、`type`、`owned_by`。
- [GetModelsForClient 实现](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/registry/model_registry.go#L1908)：读取指定客户端已注册的模型，不向模型上游发送探测请求。
- [管理账号清单字段](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/auth_files.go#L637)：包含 `id`、`auth_index`、`name`、`provider`、`disabled`、`runtime_only`。宿主 [HostAuthFileEntry 类型](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go#L752) 同样定义这些身份字段，两个布尔字段允许省略。
- [原生 ABI 方法常量](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginabi/types.go#L95) 与 [宿主回调分发](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_callbacks.go#L143)：该版本没有读取账号模型目录的宿主回调，不能构造 `host.model.list` 等不存在的方法。

## 浏览器同源读取

账号模型目录应使用页面当前 CPA 地址、反向代理前缀及内存中的管理认证，仅访问同源管理接口；不新增后台管理密钥配置或持久化管理密钥。目录读取不调用插件额度查询，不发送模型请求，不刷新 OAuth，也不读取凭据下载接口。

1. 读取 `/v0/management/auth-files`，以当前选定的账号 ID 和提供方核对目录身份。
2. 对账号 ID 使用查询参数编码，读取 `/v0/management/auth-files/models?name=<精确账号ID>`；不使用邮箱、列表位置、展示名称或推测的文件名。
3. 读取结束时核对请求代数、当前选中账号与提供方；必要时重读账号清单，确认 `auth_index` 未变化。切换账号或连接实例时使旧结果失效。
4. 限制请求时长、响应字节数、目录数量及模型标识长度。只显示候选，不自动选择、保存或启用模型唤醒。

官方模型接口的响应不回显账号身份，且其查找会取第一个 `FileName == name` 或 `ID == name` 的账号。因此其他账号的文件名若等于目标 ID，可能命中错误目录；前端必须拒绝这类别名碰撞。客户端前后核对不是宿主原子身份绑定，不能将目录读取当成发送时的授权证明。实际模型执行仍须保留原有精确账号绑定、能力检查及唤醒保护。

## 纯函数校验契约

`resolveModelAccount(data, id, provider)` 只解析清单，不执行网络请求。成功返回 `{id, provider, index}`；失败抛出 `Error`，其 `code` 为 `model_catalog_account_invalid`。

- `data.files` 必须是数组且最多 10000 项；文件条目必须是对象。
- 请求 ID 必须是非空字符串，不含 ASCII 控制字符，UTF-8 编码长度不超过 256 字节。按原值精确匹配，不转换类型或裁剪 ID。
- 目标 ID 在清单中必须恰好出现一次；请求提供方和条目的 `provider` 必须显式一致，且只允许 `codex`、`antigravity`。不以 `type` 猜测缺失的 `provider`。
- 目标账号 `disabled` 和 `runtime_only` 只允许省略或严格布尔值 `false`；`true` 和存在但类型错误的字段均拒绝。
- 目标账号的 `auth_index` 必须是非空、非纯空白字符串，不以 ID 或文件名替代。
- 任何其他条目的 `name` 精确等于目标 ID 时均拒绝，不因其提供方不同、禁用或仅运行时状态而豁免。
- 不修改调用者传入的清单，不返回额外账号元数据。

这些数量、长度、类型和提供方约束是插件客户端的保守校验，不是官方管理接口对任意输入作出的额外承诺。返回的 `index` 用于目录身份核对，不能误填为模型执行使用的 `auth_id`。

## 目录不等于实时权限

模型列表表示 CPA 当前注册的账号模型目录，不是实时上游权限或额度探测。列表非空不保证模型请求成功；列表为空也不能直接认定账号没有模型权限。读取目录不进行试发、不消耗模型生成额度；只有用户明确提交生成或唤醒后才可能发送真实模型请求。

## 验证

独立纯测试位于 `native/bridge/tests/model-catalog.test.cjs`，从现有 `panel.js` 的 CommonJS 导出读取 `resolveModelAccount`，不依赖 DOM、宿主或真实账号。

运行目录身份校验测试：

```bash
node --test native/bridge/tests/model-catalog.test.cjs
```

原生桥接回归目标 `make v2-smoke` 也包含此测试；网页交互和浏览器测试另覆盖同源请求、账号变更、编辑焦点及不自动保存的行为。
