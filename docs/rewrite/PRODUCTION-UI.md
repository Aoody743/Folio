# 正式面板 UI 同步

静态演示与正式面板共用 `frontend/src/`。正式服务器构建从 `frontend/out/` 刷新 `internal/webassets/dist/`，再将资源嵌入 Go 二进制；演示数据脚本只由 `scripts/rewrite/build-demo.py` 注入独立站点目录。

`scripts/rewrite/build.sh` 会拒绝含演示运行脚本或演示数据入口的前端输出，防止误将静态演示打包成服务器。

正式构建示例（Go 版本须与 go.mod 一致）：

```sh
GO_BIN=/path/to/go bash scripts/rewrite/build.sh --version dev-ui-current --skip-install --architectures amd64,arm64 --output /absolute/path/to/packages
python3 scripts/rewrite/test-package.py /absolute/path/to/packages/folio-dev-ui-current-linux-amd64.tar.gz
```

从实际安装包提取二进制后，运行真实服务回归：

```sh
POWERLINUX_TEST_BINARY=/absolute/path/to/extracted/folio python3 tests/rewrite/test_production_ui_browser.py
```

该测试不拦截或模拟 API：通过真实登录创建会话，在首页编辑便签、保存服务器显示名称，并重启隔离服务验证数据库持久化。同时检查正式页面没有演示入口和演示运行对象、桌面与手机布局不溢出。硬件指标来自运行该二进制的测试环境；软件和网站管理仍需在目标服务器配置执行器。

Folio v1.0 的正式包包含这套 UI。截图从当前生产界面构建和隔离演示数据生成；演示环境不会操作用户的服务器。

2026-10-23 开发包同步检查见 [ACCEPTANCE.json](evidence/ui-production-20261023/ACCEPTANCE.json)。稳定版的正式包、提交、校验和与最终浏览器验收以 [Folio v1.0 release](https://github.com/Aoody743/Folio/releases/tag/1.0.0) 附带的验收文件为准；arm64 仅交叉构建，未在 ARM 主机执行。
