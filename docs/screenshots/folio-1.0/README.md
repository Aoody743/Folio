# Folio v1.0 全页面演示截图

本图库包含 56 张 Folio v1.0 页面截图：39 张桌面页面、14 张手机页面和 3 张浏览器模式页面。面板截图由本地隔离的 Folio 服务器生成；展示用的网站、软件、文件、任务和便签数据由截图夹具提供，不会对真实服务器执行管理操作。浏览器截图在隔离服务器上实际运行 Chromium 与 Firefox WASM；MDN 页面通过服务器浏览器打开，Open Atlas 是本机演示网页。

面板桌面截图为 1440×960，手机截图为 390×844；浏览器演示截图为 1440×1100。面板图库会等待本地字体加载并检查页面宽度，滚动内容保留在各页面中。生成入口：[面板页面图库](../../../tests/rewrite/capture_panel_gallery.py) · [真实浏览器图库](../../../tests/rewrite/capture_browser_gallery.py)。

## 登录与概览

| 页面 | 截图 |
| --- | --- |
| 登录 | [00-login.png](00-login.png) |
| 主机、网站、应用与最近便签 | [01-overview.png](01-overview.png) |
| 首页便签编辑 | [38-overview-note-editor.png](38-overview-note-editor.png) |

## 网站

| 页面 | 截图 |
| --- | --- |
| 网站项目总览 | [02-sites.png](02-sites.png) |
| 网站详情 | [03-site-detail.png](03-site-detail.png) |
| 网站 TLS 与证书 | [04-site-tls.png](04-site-tls.png) |
| 网站备份 | [05-site-backups.png](05-site-backups.png) |
| 证书列表 | [06-sites-certificates.png](06-sites-certificates.png) |
| 创建网站 | [07-site-create.png](07-site-create.png) |

## 软件

| 页面 | 截图 |
| --- | --- |
| 已安装服务 | [08-software-installed.png](08-software-installed.png) |
| 软件库 | [09-software-library.png](09-software-library.png) |
| 外部数据库连接 | [10-database-connections.png](10-database-connections.png) |
| 缓存连接 | [11-cache-connections.png](11-cache-connections.png) |
| 已部署应用 | [12-deployed-apps.png](12-deployed-apps.png) |
| 浏览器应用 | [13-browser-apps.png](13-browser-apps.png) |
| 外部数据库详情 | [33-database-detail.png](33-database-detail.png) |
| 缓存连接详情 | [34-cache-detail.png](34-cache-detail.png) |
| 软件实例详情 | [35-software-detail.png](35-software-detail.png) |
| 应用详情 | [36-application-detail.png](36-application-detail.png) |

## 文件

| 页面 | 截图 |
| --- | --- |
| 文件列表 | [14-files.png](14-files.png) |
| 文件编辑器 | [15-file-editor.png](15-file-editor.png) |
| 上传文件 | [16-file-upload.png](16-file-upload.png) |
| 回收站 | [17-file-trash.png](17-file-trash.png) |

## 更多与全局设置

| 页面 | 截图 |
| --- | --- |
| 分组磁贴与退出登录 | [18-more-menu.png](18-more-menu.png) |
| 主机监控 | [19-monitor.png](19-monitor.png) |
| 终端 | [20-terminal.png](20-terminal.png) |
| 计划任务 | [21-schedules.png](21-schedules.png) |
| 通知渠道 | [22-notifications.png](22-notifications.png) |
| 任务与异常 | [23-tasks.png](23-tasks.png) |
| 共享便签 | [24-shared-notes.png](24-shared-notes.png) |
| 安全与账户 | [25-security.png](25-security.png) |
| 语言与外观 | [26-appearance.png](26-appearance.png) |
| MCP 接入 | [27-mcp.png](27-mcp.png) |
| 操作记录 | [28-audit.png](28-audit.png) |
| 高级工具 | [29-advanced-actions.png](29-advanced-actions.png) |
| 全局搜索 | [30-global-search.png](30-global-search.png) |
| 环境与维护 | [31-environment.png](31-environment.png) |
| 监控告警 | [32-monitor-alerts.png](32-monitor-alerts.png) |
| 面板与服务器名称设置 | [37-panel-settings.png](37-panel-settings.png) |

## 手机页面

| 页面 | 截图 |
| --- | --- |
| 概览 | [mobile-01-overview.png](mobile-01-overview.png) |
| 网站总览 | [mobile-02-sites.png](mobile-02-sites.png) |
| 网站详情 | [mobile-03-site-detail.png](mobile-03-site-detail.png) |
| 已安装服务 | [mobile-08-software-installed.png](mobile-08-software-installed.png) |
| 软件库 | [mobile-09-software-library.png](mobile-09-software-library.png) |
| 浏览器应用 | [mobile-13-browser-apps.png](mobile-13-browser-apps.png) |
| 文件管理 | [mobile-14-files.png](mobile-14-files.png) |
| 更多入口 | [mobile-18-more-menu.png](mobile-18-more-menu.png) |
| 计划任务 | [mobile-21-schedules.png](mobile-21-schedules.png) |
| 安全与账户 | [mobile-25-security.png](mobile-25-security.png) |
| 语言与外观 | [mobile-26-appearance.png](mobile-26-appearance.png) |
| MCP 接入 | [mobile-27-mcp.png](mobile-27-mcp.png) |
| 环境与维护 | [mobile-31-environment.png](mobile-31-environment.png) |
| 监控告警 | [mobile-32-monitor-alerts.png](mobile-32-monitor-alerts.png) |

## 浏览器模式

| 模式 | 页面 | 截图 |
| --- | --- | --- |
| 浏览器管理 | 两种模式的安装入口 | [browser-modes-installed.png](browser-modes-installed.png) |
| 面板浏览器 | 服务器 Chromium 打开 MDN HTTP 指南 | [panel-browser-mdn.png](panel-browser-mdn.png) |
| Firefox WASM | 完整 Firefox 打开本机 Open Atlas 演示站 | [firefox-wasm-atlas.png](firefox-wasm-atlas.png) |

历史发行截图保留在 [beta7 图库](../panel-1.0-beta7/README.md) 与 [beta1 图库](../panel-1.0-beta1/README.md)。
