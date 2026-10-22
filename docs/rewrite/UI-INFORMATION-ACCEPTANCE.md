# UI 信息重构验收记录

2026-10-22。对应开发验证包 `1.0-beta5-ui-qa`，已完成源码修改和发行准备，**没有发布新 beta，也没有覆盖 beta4 资产**。安装器逻辑未改动。

## 最终交互

- 五项底栏保持原顺序；手机支持自然下滑，顶栏只显示品牌与“查看任务”，电脑额外显示账户按钮。
- 语言、三套字体、五套配色位于“更多 → 语言与外观”；登录页保留语言选择。全局搜索移入“更多”，快捷键继续可用。
- 首页只保留主机、网站、应用汇总；便签、任务、异常和记录进入二级页。
- 软件默认已安装服务；网站、文件及应用使用分页列表；资源详情使用独立页面。应用日志点开即读取，读取或提交失败留在对应上下文提示。
- hash 路由支持直达、刷新和返回，保留列表条件；跨运维页面清除上一类资源定位。文件草稿保留，未保存时保护文件及目录切换。

## 自动验证结果

| 检查 | 结果 |
| --- | --- |
| 前端单元测试 | 93 项全部通过，包含翻译覆盖、路由白名单、分页、未知/冲突应用状态、任务观察、幂等及 SQL 精度 |
| Go 全量竞态测试 | `go test -race ./...` 通过 |
| Go 静态检查 | `go vet ./...` 通过 |
| 网站授权统计 | 实际 Core/Provider 集成测试通过：授权、查询、分类、状态、分页及未知值，不泄漏不可见资源 |
| 构建 | TypeScript、Next 静态导出、Linux amd64/arm64 构建通过 |
| 实际 amd64 包 | 私有 prefix 安装/重复安装、HTTP 健康/会话、嵌入静态资源、运行锁、退出、重装及保留数据卸载通过 |
| 真实便签 API/MCP/Chromium | 创建、SSE、历史、并发冲突、合并、离线恢复、来源标签与 XSS 检查通过 |
| 真实文件 API/MCP/Chromium | 新建、CodeMirror、哈希冲突、历史还原、离线恢复、GB18030、大文本流式保存、5 MiB 中断续传与下载摘要通过 |
| 真实计划任务 | 成功/失败结果、重启持久化、关联任务与策略表单通过 |
| 中英界面 | 登录输入保留、运行时切换、浏览器偏好、用户原文与英文界面检查通过 |
| 交叉界面矩阵 | 中英 × 三套字体 × 五套配色，共 30 组；每组覆盖五种尺寸，本地中文字体实际加载成功，无外部字体请求 |
| 导航与操作 | 长名称与多页、直达/刷新/返回、筛选、创建/安装/上传/计划入口、三次点击查看应用日志、可见读取失败、全局搜索入口、跨模块定位及草稿保护通过 |
| 既有安装器文案同步 | `sync-installer-ui.py --check` 通过 |

五种尺寸：1366×768、1440×900、390×844、360×640、320×568。桌面主列表检查正常字号下可分页操作；手机允许滚动，保留完整文字与操作入口。浏览器验证使用 Chromium 的桌面与移动尺寸，并非手机硬件键盘验证。ARM64 已构建，本轮运行验证使用 amd64。

大集合网站、应用的数据使用**明确的展示夹具**，用于长名称、未知状态和多页渲染；统计的权限与过滤另由 Go 集成测试验证。文件、便签、任务与包安装使用真实隔离服务。截图中的主机为测试环境，网站/应用数量不是线上资产或公网健康证明。

## 截图

- [中文桌面概览](../screenshots/panel-ui-information/zh-CN-overview-1366.png)
- [中文手机概览](../screenshots/panel-ui-information/zh-CN-overview-390.png)
- [英文窄屏概览](../screenshots/panel-ui-information/en-overview-320.png)
- [网站列表](../screenshots/panel-ui-information/list-sites-1366.png) · [文件列表](../screenshots/panel-ui-information/list-files-390.png)
- [手机更多入口](../screenshots/panel-ui-information/zh-more-mobile.png) · [手机语言与外观](../screenshots/panel-ui-information/zh-appearance-mobile.png)

## 开发验证包摘要

这些是当前开发树生成的 QA 包，未创建公开 release/tag；正式发行应从确定的干净提交重新构建。

```text
a1ecef440b397e1dcb29a9eb46790723d3765f10c6fb8bc6dd400fdcc432d6d8  folio-1.0-beta5-ui-qa-linux-amd64.tar.gz
75d79688f84d7a18bc5f2fa5ba24f7ae559313e6703139fdb4e071287fdba5a5  folio-1.0-beta5-ui-qa-linux-arm64.tar.gz
```

复测入口：`npm --prefix frontend test`、`go test -race ./...`、`tests/rewrite/test_information_browser.py` 及现有便签、文件、双语、计划任务浏览器脚本。浏览器脚本的 `POWERLINUX_TEST_BINARY` 应指向实际打包后的 Folio 可执行文件。
