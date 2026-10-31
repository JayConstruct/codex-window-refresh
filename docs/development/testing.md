# 开发检查与发布

命令均从仓库根目录执行。Go 需要 1.23+，SQLite 测试需要 CGO 和 C 编译器；网页 Node 测试需要 Node.js 18+，安装器测试需要 Python 3 与 PyYAML。V2 原生依赖见 [架构与构建](architecture.md#构建)。

## 检查入口

| 命令 | 范围 |
| --- | --- |
| `make docs-check` | README、分类文档与测试索引的相对文件链接 |
| `make check` | 文档链接、全部 Go 测试及 vet，包含 V1/V2 |
| `make race` | 全部 Go 竞态检查 |
| `make panel-check` | V1/V2 网页 Node 测试 |
| `make v1-panel-check` / `make v2-panel-check` | 仅运行指定版本网页测试 |
| `make installer-check` | 构建 V1 策略检查命令后，运行 Python 安装与打包工具测试 |
| `make v2-smoke` | 构建 V2，运行网页测试与原生假宿主端到端验证 |
| `make v2-panel-browser-check` | Chromium 桌面与移动端 V2 页面验证 |
| `make release-check` | 两代版本文件、内置版本、客户端校验和及发布说明 |
| `make v1-release-check` / `make v2-release-check` | 仅检查指定版本发布元数据 |

浏览器验证使用系统 Chromium/Chrome，可通过 `CHROME_BIN` 指定可执行文件。测试使用固定样本、临时数据库和假宿主；真实账号查询、模型发送、systemd 实机安装及负载验证的完成情况须另行记录，不能由单元测试通过推断。

`CWR_AUTH_ONLY=1 make v2-panel-browser-check` 单独检查自动登录：CPA 凭据复用、桌面及移动端勾选项、手动登录是否记住、刷新恢复、主动断开和认证失效后的退出，以及 CPA 登录存储保持不变。

`test_codeman_client.py` 验证 Codeman 注册表接入、原配置及参数保留、重复配置、私有备份与失败不覆盖。本机安装了 Codeman 时还会使用其实际注册表和命令渲染器，在临时目录中运行假 Codex，验证项目头、模型、推理强度、恢复会话及退出码；可用 `CWR_TEST_CODEMAN_APP` 指定 Codeman 应用目录，不修改真实注册表或启动真实模型请求。

## 构建与测试归属

- `make v2-build` 生成 `bin/window-refreshd` 和 `bin/codex-window-refresh-v2-bridge.so`。
- `make build` 生成 V1 策略检查命令；`make plugin` 生成 V1 Go 动态库；`make native-smoke` 验证 V1 C ABI 加载。
- Go 测试跟随对应包，网页 Node 测试位于 `tests/web/v1/` 和 `tests/web/v2/`。
- `native/bridge/tests/smoke.py` 负责 V2 C 接入层与真实后台进程的端到端测试。
- `scripts/tests/` 验证 Python 安装、打包和文档工具；浏览器检查脚本保留在 `scripts/`。
- `make clean` 清除本地构建、打包、Python 缓存与 Go 测试分析产物。

共享的 `internal/core`、`internal/cpa` 仍被 V2 引用，不能随 V1 文档一并当作旧实现删除。安装脚本的公开下载路径保持稳定，移动测试或文档时需同步 Makefile、CI 触发路径和打包源路径。

## 发布

| 组件 | 版本文件 | 校验与打包 | 工作流 |
| --- | --- | --- | --- |
| V1 | `release/version.txt` | `scripts/release_check.py`、`scripts/package-release.py` | `.github/workflows/prerelease.yml` |
| V2 后台与客户端 | `release/v2-version.txt` | `scripts/package-v2.py --check`、`scripts/package-v2.py` | `.github/workflows/v2-prerelease.yml` |
| V2 接入层 | `native/bridge/version.h` | 独立版本写入 V2 包元数据 | 随 V2 包构建 |

修改对应版本文件、内置版本和 `docs/releases/<版本>.md` 后运行发布检查。V2 客户端同时校验安装器中的版本与脚本 SHA-256；接入层版本不要求与后台相同。工作流在 amd64/arm64 完成构建、测试、打包及浏览器检查后发布预发布包；已有发布资产保持不变。重试可使用对应工作流的手动入口。

文档检查工作流独立响应文档变更，不依赖递增发布版本。包内文档保留安装器认可的名称，通过打包时转换链接引用本地文件或固定源码提交。新增包内文档时同时更新对应安装器的文件白名单。
