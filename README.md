<p align="center">
  <img src="./frontend/public/folio-logo.png" alt="Folio logo" width="160">
</p>

# Folio · 折纸面板

Folio 是一款面向 Linux 服务器的管理面板，提供主机状态、网站与软件管理、文件和终端、任务记录，以及可与同一权限和审计体系协作的 MCP 接口。

新版使用 Go 服务和静态 Web 界面，主机上无需常驻 Node.js、Python 或 AI 模型进程。独立主机执行器按需启用。

## 当前版本

[Folio 1.0-beta1](https://github.com/Aoody743/Folio/releases/tag/1.0-beta1) 已发布，提供 Linux amd64 安装包和一键安装脚本。

## 一键安装

在全新 Linux amd64、运行 systemd 的主机上执行：

```sh
curl -fsSL https://github.com/Aoody743/Folio/releases/download/1.0-beta1/install-1.0-beta1.sh | sudo bash
```

安装脚本会下载该版本的软件包及 `SHA256SUMS.txt`，校验 SHA-256 后调用包内安装器。运行前可以先审阅[一键安装脚本](scripts/rewrite/install-1.0-beta1.sh)。安装目标为 Linux amd64 systemd 主机；可选 Agent 能力默认关闭，需要显式参数和 root 私有策略。

## 本地开发

需要与仓库匹配的 Go、Node.js 24 和 npm：

```sh
make rewrite-deps
make rewrite-test
make rewrite-build REWRITE_VERSION=dev
make rewrite-dev
```

构建、手动安装、升级、恢复和开发环境说明见[新版构建与安装手册](scripts/rewrite/README.md)。

## 面板截图

截图覆盖当前面板的 18 个可见页面，包括证书、数据库连接、缓存空间和通知渠道。页面状态使用隔离的示例数据；软件目录及其验证状态取自随 beta1 发布的产品清单，不代表真实主机快照。查看[截图索引](docs/screenshots/panel-1.0-beta1/README.md)。

![Folio 面板页面总览](docs/screenshots/panel-1.0-beta1/contact-sheet.png)

## 产品文档

- [1.0-beta1 发布说明](.github/release-notes/1.0-beta1.md)
- [开发计划](docs/rewrite/DEVELOPMENT-PLAN.md)
- [MCP 接入设计](docs/rewrite/AI-NATIVE-MCP.md)
- [界面设计规范](docs/rewrite/UI-SPEC.md)
- [本地开发交接](docs/rewrite/HANDOFF.md)

仓库仍包含上一代实现和兼容标识；新版界面、产品名称和新发行命令统一使用 **Folio**。

## License

本项目采用 Apache License 2.0，详见 [LICENSE](./LICENSE)。
