# Folio 当前发行与验收入口

更新：2026-10-23。当前稳定版为 **Folio v1.0**，发行版本号 `1.0.0`。完整源码、安装包和验收记录以 GitHub 上的 [`1.0.0` release](https://github.com/Aoody743/Folio/releases/tag/1.0.0) 为准。

## v1.0 发行范围

- 提供 Linux amd64、arm64 发行包，以及面向 amd64 的中英双语一键安装脚本。arm64 包已交叉构建；本轮发行验收在 amd64 环境完成。
- 受管应用的运行支持目标为 Ubuntu 24.04 amd64 + systemd。此次没有重新执行 systemd 虚拟机升级/回退测试；历史记录不会当作本轮执行结果。
- 登录、主机概览、网站、软件、文件、运维和设置页面采用统一的 Astryx 定制规范，并继续支持中英文、三套字体预设和五套主题。
- 网站项目按静态、PHP、Git、代理分类；软件库、文件操作、最近便签、顶部服务器身份和“更多”页面按当前布局呈现。
- 面板 Chromium 与 Firefox WASM 可独立选择安装。面板浏览器在服务器运行；Firefox WASM 通过面板认证的 Wisp 使用服务器网络，需要 HTTPS 和支持 JSPI 的客户端。
- 默认安装只监听回环地址。更改监听范围前请配置并验证 HTTPS 与允许的来源；真实 DNS provider/公开 CA 不在 v1.0 本轮验收范围内。

验收记录、包校验和及验证日志随 [GitHub v1.0 release](https://github.com/Aoody743/Folio/releases/tag/1.0.0) 提供。全部当前面板与浏览器截图见 [Folio v1.0 截图索引](../screenshots/folio-1.0/README.md)。截图中的服务器、网站、软件、文件和便签数据都是演示数据。

## 历史预发布记录

以下文档记录 beta1 至 beta7 阶段的实现和当时验收，不代表 v1.0 中重新执行过相同平台检查：

- [beta7 双浏览器与导航验收](BROWSER-APPS-BETA7.md)
- [beta5 UI 信息验收](UI-INFORMATION-ACCEPTANCE.md)
- [beta4 安装与界面体验](BETA4-ACCEPTANCE-20261022.md)
- [beta3 审计整改验收](BETA3-ACCEPTANCE-20261022.md)

备份与恢复采用流式处理、完整性校验和取消能力，不设置固定的备份总大小上限。计划任务保留重叠/错过执行策略和最终任务状态核验；操作和任务结果按当前用户权限读取。更详细的历史设计与限制见上面的版本记录。
