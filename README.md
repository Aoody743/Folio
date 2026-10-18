# Folio · 折纸面板

![Folio](./frontend/public/icon.svg)

Folio 是一款面向 Linux 服务器的管理面板。它提供主机状态、网站与软件管理、文件与终端、任务记录，以及可与同一权限和审计体系协作的 MCP 接口。

新版使用 Next.js 静态界面、Go 服务和可选的独立主机执行器。服务器不需要常驻 Node.js、Python 或 AI 模型进程。

## 当前状态

当前分支仍处于首发候选阶段，**尚未正式发布**。发行范围、验收结果和未完成门槛见[首发就绪清单](docs/rewrite/LAUNCH-READINESS-20261018.md)。

## 本地开发

```sh
make rewrite-deps
make rewrite-test
make rewrite-build REWRITE_VERSION=dev
make rewrite-dev
```

构建、安装、升级和恢复说明见[新版构建与安装手册](scripts/rewrite/README.md)。正式发行前，请勿将开发构建安装到生产主机。

## 产品文档

- [开发计划](docs/rewrite/DEVELOPMENT-PLAN.md)
- [上线前差距清单](docs/rewrite/LAUNCH-READINESS-20261018.md)
- [MCP 接入设计](docs/rewrite/AI-NATIVE-MCP.md)
- [界面设计规范](docs/rewrite/UI-SPEC.md)
- [本地开发交接](docs/rewrite/HANDOFF.md)

仓库仍包含上一代实现和安装兼容标识；新版界面、产品名称及新发行命令统一使用 **Folio**。兼容标识不作为产品展示名称。

## License

本项目采用 Apache License 2.0，详见 [LICENSE](./LICENSE)。
