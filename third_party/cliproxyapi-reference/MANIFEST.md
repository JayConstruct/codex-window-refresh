# CLIProxyAPI 官方接口来源清单

来源：[`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI)，固定提交 [`3de4e248f6bb12fc7dfafb183404f3b248578922`](https://github.com/router-for-me/CLIProxyAPI/commit/3de4e248f6bb12fc7dfafb183404f3b248578922)，提交时间 2026-10-10T17:16:32Z；读取日期 2026-10-11。

公开仓库仅保留本来源清单和上游 MIT [LICENSE](LICENSE)。实现核对时使用过的上游源码快照保留在本地并由 `.gitignore` 排除，不是本插件的构建依赖，不随公开源码包分发。部分上游文件包含硬编码 OAuth 客户端凭据，因此公开发布使用固定提交的来源链接供查阅，而不复制这些源码。没有编译或运行完整官方宿主；源码中的字段定义不代表本地部署已启用对应能力。

2026-10-19 补充只读核对同一提交的认证文件状态管理：`auth_files_fields.go`、`auth_files.go` 与 `server_management.go`，用于验证独立启停端点和文件名/账号索引身份限定。此次不下载或发布完整上游源码，具体边界见 [V2 账号开关说明](../../docs/backend-v2-account-controls.md)。

以下列出核对过的上游路径、Git blob SHA 和固定源码链接；表中路径不表示公开仓库包含该文件。构建与版本兼容应以目标部署的实际提交重新验证。

| 上游路径 | Git blob SHA | 上游固定链接 |
| --- | --- | --- |
| `LICENSE` | `e3305a12a6147f3eb16d4bf0057e89819892d626` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/LICENSE) |
| `go.mod` | `145d668e2a7a23d5a99fd640ad52ee393f18b1b2` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/go.mod) |
| `sdk/pluginapi/types.go` | `6060977886cd9c24d5493dc873c9ba1858af5ac8` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginapi/types.go) |
| `sdk/pluginabi/types.go` | `e79fd8c16fe73dbad1ac9f38ec7900167740c455` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginabi/types.go) |
| `sdk/pluginhost/quota.go` | `8c98905bbb3aaffe18a79ad3ab1813466e5c1283` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/pluginhost/quota.go) |
| `sdk/cliproxy/executor/types.go` | `d178957dfa4773ebb9ef9c527d58e9294f5cfa1e` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/executor/types.go) |
| `sdk/cliproxy/session/identity.go` | `d0dcd91b642055b9d9c2117afe9cd87f4afa90fc` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/session/identity.go) |
| `sdk/api/handlers/handlers.go` | `61604ee23780d8ce688a83cac11c282d967d24af` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers.go) |
| `sdk/api/handlers/handlers_interceptors.go` | `734232bc92aa7a453f9f28ecadd3ca8111901411` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers_interceptors.go) |
| `sdk/api/handlers/model_execution.go` | `3a24476cca40b191afa6921155499ba0d05767e7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/model_execution.go) |
| `internal/access/config_access/provider.go` | `b6d371ae1540e5b359d3c81c5dbcdf41c303a361` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/access/config_access/provider.go) |
| `internal/api/server_middleware.go` | `f1696228e3729f874755c1bc143aa66a6d12e062` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/server_middleware.go) |
| `internal/pluginhost/host_callbacks.go` | `4af872a9f83feb6df7de6ccef14a70022b0e8db7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_callbacks.go) |
| `internal/pluginhost/host_model_stream_callbacks.go` | `ad3184c4013059a6e59883cfd980a87b85bec5f7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/host_model_stream_callbacks.go) |
| `internal/pluginhost/auth_callbacks.go` | `558b67d9d4ede2bad058a7d527866e0bfd9e7f24` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/auth_callbacks.go) |
| `internal/pluginhost/adapters_interceptors.go` | `055d5254d01307ec813d36b3ec67e5fde24b50d7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/adapters_interceptors.go) |
| `internal/pluginhost/adapters_usage_translation.go` | `f239d2be84c0452443c2b1449fc5ec59086e1001` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/adapters_usage_translation.go) |
| `internal/pluginhost/loader_unix.go` | `412aa4676d08588c1d046a5adb83f6a24dfe0a9c` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/loader_unix.go) |
| `internal/pluginhost/rpc_schema.go` | `188ed246c73e7973573f68d9fdda58292723d1c6` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/rpc_schema.go) |
| `internal/pluginhost/rpc_client.go` | `96e3785d25963a40c676f3b4ae20c08339c09090` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/rpc_client.go) |
| `internal/runtime/executor/helps/codex_quota.go` | `cc0c1c2b4f4e43b47a60ffdd09eaccc46cd5eec3` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/runtime/executor/helps/codex_quota.go) |
| `examples/plugin/README.md` | `502be19834e14a7a2149b2c1cd8b9deff5510415` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/README.md) |
| `examples/plugin/simple/README.md` | `87f1b19a546f649ab8581cb9a2b5cb87459d5b38` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/simple/README.md) |
| `examples/plugin/simple/go/main.go` | `ca958f5de7f72e3d0212ea3de2caad0713fe9b33` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/simple/go/main.go) |
| `examples/plugin/host-callback/go/main.go` | `8c004f78540b19c79cb1c608d63f775ad6870743` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/host-callback/go/main.go) |
| `examples/plugin/host-model-callback/go/main.go` | `6dc93fd9031837a25320115e12634348737a0df6` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/host-model-callback/go/main.go) |
| `examples/plugin/host-model-callback/README.md` | `f0b5c3929fc0ad7bef0429bc8f81d3c00a2ca3d7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/host-model-callback/README.md) |
| `examples/plugin/request-lifecycle/README.md` | `2f8a1aa4d92bc1a34b891d2561943f33be595142` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/request-lifecycle/README.md) |
| `examples/plugin/request-lifecycle/go/main.go` | `bbef503ed36ea381c996fcb9b0308164e7709348` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/request-lifecycle/go/main.go) |
| `examples/plugin/request-lifecycle/go/go.mod` | `420d628e86758bf66037d867be5beadc0db4bf7d` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/request-lifecycle/go/go.mod) |
| `examples/plugin/usage/go/main.go` | `80f8197e2ddc88f00361241e37f63e3ca78a7ecb` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/examples/plugin/usage/go/main.go) |
| `sdk/cliproxy/auth/conductor_execution.go` | `94953120258d375f6373ebd7716a14bd6f00759a` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/cliproxy/auth/conductor_execution.go) |
| `sdk/api/handlers/handlers_execution.go` | `16c0f4f781cab4e57e69f86f8f3dc1d9285f59dc` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/sdk/api/handlers/handlers_execution.go) |
| `internal/pluginhost/http_bridge.go` | `40b15ef488560dbbc48effdf87c61bdd6a99cd83` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/pluginhost/http_bridge.go) |
| `internal/api/handlers/management/api_tools.go` | `c03020527a84287a0c066e692ca6ff18020232f7` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/api_tools.go) |
| `internal/api/handlers/management/plugin_quota.go` | `be8d1e731dc1fa68dbc5690ee91e528febf90a28` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/plugin_quota.go) |
| `internal/api/server_management.go` | `12a2ba53715c24ac5beb51d43651bf9b6f139753` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/server_management.go) |
| `internal/api/handlers/management/auth_files_fields.go` | `7304b06d90fcfa59198890e39da70ab0d436c87d` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/auth_files_fields.go) |
| `internal/api/handlers/management/auth_files.go` | `0a4ed74f70f5d62c9ed74988334239ec05c6c587` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/handlers/management/auth_files.go) |
| `internal/api/server_management_v8.go` | `7f68dad013406a70ebee6f34c4ba1daae096568a` | [source](https://github.com/router-for-me/CLIProxyAPI/blob/3de4e248f6bb12fc7dfafb183404f3b248578922/internal/api/server_management_v8.go) |

