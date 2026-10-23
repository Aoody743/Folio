<p align="center">
  <img src="./frontend/public/folio-logo.png" alt="Folio logo" width="160">
</p>

# Folio · 折纸面板

Folio 是一款用于管理 Linux 服务器的 Web 面板。它把主机状态、网站与软件、文件与终端、计划任务、账户安全和操作审计放在同一个界面中；支持 MCP 的客户端也可以在授权后调用面板提供的能力。

服务端使用 Go，管理界面以静态资源嵌入发行包。运行 Folio 不需要在服务器上常驻 Node.js、Python 或 AI 模型进程。可选的主机执行器默认关闭，只有配置 root 私有策略后才会启用。

## 功能

- 查看主机概览、资源指标、服务状态和共享便签。
- 管理网站、证书和软件目录中的服务。
- 浏览文件，使用受控终端和计划任务。
- 查看数据库与缓存连接、任务进度和操作记录。
- 管理账户安全、会话、MCP 客户端授权和界面主题。
- 通过服务器浏览器访问网页，支持标签页、鼠标键盘、中文输入与手机操作；网页请求使用服务器的网络出口。

网站与数据库备份不再设置固定总大小上限，仍使用流式处理和完整性校验。计划任务支持数据库备份、最终任务结果、失败通知、重叠执行和错过执行策略。

安装向导和面板支持中英双语。面板可在登录页及“更多 → 语言与外观”切换语言并记住浏览器偏好；切换保留当前页面和输入，资源名称与用户内容保持原文。首页空状态、首次设置、软件变更摘要和任务结果也已完善。

两种浏览器模式可在“软件 → 浏览器”独立安装或卸载，默认均不安装。面板模式在服务器运行 Chromium；Firefox WASM 提供完整 Firefox 界面，通过面板的认证 Wisp 出口访问网页，需要 HTTPS 和支持 JSPI 的客户端。安装后可从“更多 → 运维”打开。完整功能和环境说明见[服务器浏览器](docs/rewrite/SERVER-BROWSER.md)。

## Folio v1.0 界面截图

以下 56 张截图来自 Folio v1.0 的统一界面：39 张桌面页面、14 张手机页面，以及 3 张浏览器模式页面。网站、服务、文件、便签与监控数据均为演示数据，不代表真实服务器状态。查看[全部页面截图与索引](docs/screenshots/folio-1.0/README.md)，或打开[交互式演示面板](https://folio-ui-demo-20261023.workspace-734782.chatgpt.site/)体验布局。

### 概览

[![Folio v1.0 主机概览与最近便签](docs/screenshots/folio-1.0/01-overview.png)](docs/screenshots/folio-1.0/01-overview.png)

### 网站

[![Folio v1.0 网站项目卡片](docs/screenshots/folio-1.0/02-sites.png)](docs/screenshots/folio-1.0/02-sites.png)

### 软件

[![Folio v1.0 软件库](docs/screenshots/folio-1.0/09-software-library.png)](docs/screenshots/folio-1.0/09-software-library.png)

### 文件

[![Folio v1.0 文件管理](docs/screenshots/folio-1.0/14-files.png)](docs/screenshots/folio-1.0/14-files.png)

## 浏览器模式截图

浏览器截图展示两个独立模式：面板浏览器通过服务器网络打开 MDN 的 HTTP 指南；Firefox WASM 打开本机托管的 Open Atlas 演示网页。

### 可选安装入口

[![浏览器应用安装与管理](docs/screenshots/folio-1.0/browser-modes-installed.png)](docs/screenshots/folio-1.0/browser-modes-installed.png)

### 面板浏览器：MDN HTTP 指南

[![面板 Chromium 浏览器打开 MDN HTTP 指南](docs/screenshots/folio-1.0/panel-browser-mdn.png)](docs/screenshots/folio-1.0/panel-browser-mdn.png)

### Firefox WASM：Open Atlas

[![Firefox WASM 浏览器打开 Open Atlas 演示网页](docs/screenshots/folio-1.0/firefox-wasm-atlas.png)](docs/screenshots/folio-1.0/firefox-wasm-atlas.png)

Firefox 截图中的 Open Atlas 是截图验收时在本机提供的演示网页，用于展示完整 Firefox 界面和 Wisp 浏览路径；它不是线上网站。

## 安装与首次登录

当前稳定版是 [Folio v1.0](https://github.com/Aoody743/Folio/releases/tag/1.0.0)，提供 Linux amd64、arm64 安装包，一键安装面向 amd64。ARM64 为交叉构建，运行支持目标为 Ubuntu 24.04 amd64 + systemd。在服务器上执行：

```sh
curl -fsSL https://github.com/Aoody743/Folio/releases/download/1.0.0/install-1.0.0.sh | sudo bash -s -- --lang zh-CN
```

英文安装使用 `--lang en`，无人值守增加 `--yes`；面板语言可在登录页及“更多 → 语言与外观”切换。

安装脚本会下载发行包和 `SHA256SUMS.txt`，校验发行包后再运行包内安装器。可以先查看[一键安装脚本](scripts/rewrite/install-1.0.0.sh)及[发行校验清单](https://github.com/Aoody743/Folio/releases/download/1.0.0/SHA256SUMS.txt)。

安装完成后，服务由 systemd 管理，默认只监听服务器本机的 `127.0.0.1:9000`。在自己的电脑上建立 SSH 隧道：

```sh
ssh -L 9000:127.0.0.1:9000 your-user@your-server
```

保持隧道运行，在浏览器打开 <http://127.0.0.1:9000>。首次设置时，按页面提示从服务器本机管理员控制台取得私有初始化令牌，并设置管理员账号和密码。令牌保存在 `/var/lib/powerlinux/bootstrap-token`；完成设置后会被删除。不要把令牌或密码放进 URL、聊天记录或命令行参数。

可以用下面的命令查看服务状态和健康状态：

```sh
sudo systemctl status powerlinux.service
sudo /opt/powerlinux/current/folio healthcheck \
  --server-config /etc/powerlinux/server.json \
  --expected-version 1.0.0
```

默认配置不向公网开放服务。如需通过其他入口访问，请先按[安装与运维手册](scripts/rewrite/README.md)配置并验证 HTTPS、监听地址和允许的来源。

## 更新、回退与卸载

安装器会保留版本快照，并提供更新、回退和卸载流程。卸载会停止并移除 Folio 的服务入口，同时保留数据、配置和快照，方便按需恢复或清理。请按[安装与运维手册](scripts/rewrite/README.md#update-rollback-and-uninstall)中的步骤执行这些操作。

## 本地开发

开发需要仓库指定版本的 Go、Node.js 24 和 npm：

```sh
make rewrite-deps
make rewrite-test
make rewrite-build REWRITE_VERSION=dev
make rewrite-dev
```

常用入口：

- `cmd/powerlinux`：服务器与命令行程序。
- `internal/`：API、授权、任务和主机能力实现。
- `frontend/`：Next.js 静态管理界面。
- `scripts/rewrite/`：构建、安装、升级、回退和卸载工具。

完整参数和运维说明见[构建与安装手册](scripts/rewrite/README.md)。

## 项目文档

- [Folio v1.0 发布说明](.github/release-notes/1.0.0.md)
- [Folio v1.0 发布验收记录](https://github.com/Aoody743/Folio/releases/download/1.0.0/ACCEPTANCE.json)
- [当前版本与验收范围](docs/rewrite/CURRENT-RELEASE.md)
- [UI 优化实施记录](docs/rewrite/UI-POLISH-BATCHES.md)
- [v1.0 全部页面截图索引](docs/screenshots/folio-1.0/README.md)
- [beta7 面板与浏览器完整截图索引](docs/screenshots/panel-1.0-beta7/README.md)
- [beta1 历史面板截图](docs/screenshots/panel-1.0-beta1/README.md)
- [MCP 接入设计](docs/rewrite/AI-NATIVE-MCP.md)
- [界面设计规范](docs/rewrite/FOLIO-DESIGN-SYSTEM.md)
- [开发计划](docs/rewrite/DEVELOPMENT-PLAN.md)

仓库仍包含旧版兼容代码；新版界面、产品名称及安装命令统一使用 **Folio**。

## License

本项目采用 Apache License 2.0，详见 [LICENSE](LICENSE)。
