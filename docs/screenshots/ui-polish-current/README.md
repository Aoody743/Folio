# Folio UI 优化开发版截图

本图库使用 `1.0-beta7-ui-batch3` 开发验证包，涵盖 33 张桌面页面、8 张手机页面和 3 张浏览器实测截图，共 44 张。截图截取实际视口，长页面与手机列表允许下滑。公开发行仍为 **1.0-beta7**，这套界面尚未发行新 beta。

面板列表、指标、网站、应用、文件、任务和便签使用明确的只读展示夹具；认证和内置软件目录来自隔离服务。这些展示数据不证明真实主机健康。两种浏览器模式使用实际安装组件与服务器网络：面板浏览器访问 MDN，Firefox WASM 通过 Wisp 打开本机 Open Atlas 演示网页。

生成脚本：[面板图库](../../../tests/rewrite/capture_panel_gallery.py)、[浏览器图库](../../../tests/rewrite/capture_browser_gallery.py)。开发构建含未提交工作树，未将这些验证包作为正式发行资产。真实接口、任务与权限检查另见[分批实现记录](../../rewrite/UI-POLISH-BATCHES.md)。

## 桌面页面

### 登录与概览

| 页面 | 截图 |
| --- | --- |
| 登录 | [00-login.png](00-login.png) |
| 主机概览 | [01-overview.png](01-overview.png) |

### 网站

| 页面 | 截图 |
| --- | --- |
| 网站列表 | [02-sites.png](02-sites.png) |
| 网站详情 | [03-site-detail.png](03-site-detail.png) |
| 网站 TLS 与证书 | [04-site-tls.png](04-site-tls.png) |
| 网站备份 | [05-site-backups.png](05-site-backups.png) |
| 证书列表 | [06-sites-certificates.png](06-sites-certificates.png) |
| 创建网站 | [07-site-create.png](07-site-create.png) |

### 软件与应用

| 页面 | 截图 |
| --- | --- |
| 已安装服务 | [08-software-installed.png](08-software-installed.png) |
| 软件库 | [09-software-library.png](09-software-library.png) |
| 外部数据库连接 | [10-database-connections.png](10-database-connections.png) |
| 缓存连接 | [11-cache-connections.png](11-cache-connections.png) |
| 部署的应用 | [12-deployed-apps.png](12-deployed-apps.png) |
| 浏览器应用安装管理 | [13-browser-apps.png](13-browser-apps.png) |

### 文件

| 页面 | 截图 |
| --- | --- |
| 文件列表 | [14-files.png](14-files.png) |
| 文件编辑器 | [15-file-editor.png](15-file-editor.png) |
| 上传文件 | [16-file-upload.png](16-file-upload.png) |
| 回收站 | [17-file-trash.png](17-file-trash.png) |

### 更多

| 页面 | 截图 |
| --- | --- |
| 更多入口 | [18-more-menu.png](18-more-menu.png) |
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

## 手机页面

| 页面 | 截图 |
| --- | --- |
| 概览 | [mobile-01-overview.png](mobile-01-overview.png) |
| 网站列表 | [mobile-02-sites.png](mobile-02-sites.png) |
| 已安装服务 | [mobile-08-software-installed.png](mobile-08-software-installed.png) |
| 文件列表 | [mobile-14-files.png](mobile-14-files.png) |
| 更多入口 | [mobile-18-more-menu.png](mobile-18-more-menu.png) |
| 语言与外观 | [mobile-26-appearance.png](mobile-26-appearance.png) |
| 环境与维护 | [mobile-31-environment.png](mobile-31-environment.png) |
| 监控告警 | [mobile-32-monitor-alerts.png](mobile-32-monitor-alerts.png) |

## 浏览器模式

| 模式 | 页面 | 截图 |
| --- | --- | --- |
| 浏览器管理 | 可选安装与管理 | [browser-modes-installed.png](../ui-polish-browsers/browser-modes-installed.png) |
| 面板浏览器 | MDN HTTP 指南 | [panel-browser-mdn.png](../ui-polish-browsers/panel-browser-mdn.png) |
| Firefox WASM | Open Atlas 本机演示站点 | [firefox-wasm-atlas.png](../ui-polish-browsers/firefox-wasm-atlas.png) |

已发行版本保留：[beta7 图库](../panel-1.0-beta7/README.md)、[beta1 历史图库](../panel-1.0-beta1/README.md)。
