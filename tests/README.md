# 测试目录

| 位置 | 对象 | 命令 |
| --- | --- | --- |
| `web/v1/*.test.cjs` | V1 内嵌面板 | `make v1-panel-check` |
| `web/v2/*.test.cjs` | V2 后台内嵌面板 | `make v2-panel-check` |
| `../native/bridge/tests/smoke.py` | V2 原生接入与后台端到端 | `make v2-smoke` |
| `../scripts/tests/test_*.py` | 安装、打包与文档工具 | `make installer-check` |
| 各 Go 包内 `*_test.go` | 领域、适配、存储与 API | `make check` / `make race` |

网页测试归属于各版本页面，与原生接入层的加载测试分开。`make panel-check` 运行两代网页测试，浏览器与发布流程见 [开发检查与发布](../docs/development/testing.md)。
