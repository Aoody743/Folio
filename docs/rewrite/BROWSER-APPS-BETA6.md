# beta6 可选浏览器应用验收

日期：2026-10-23。基于已有界面信息重构，增加两种独立可选安装的浏览器模式。beta5 为准备期标签；公开发行使用 beta6。

## 实现

- 入口：软件 → 应用服务 → 浏览器应用。默认两者均未安装；安装后可在更多 → 运维中打开。
- 面板模式：服务器 Chromium、私有 CDP pipe、临时隔离配置。可复用系统浏览器；Linux amd64 缺少时按固定 SHA-256 下载并验证独立 Chrome headless shell。
- Firefox WASM：Puter 上游 v0.0.1，按需下载及安全解包；完整原生界面通过服务器认证 Wisp 出口访问。移除公共 Wisp 与外部字体请求。启动检测 HTTPS/JSPI，预留返回面板与全屏栏。
- 安装与卸载经过任务服务、当前权限、修订号和防重复提交。重启保留安装状态，未完成任务显示中断，可检查后重试。
- 独立卸载关闭所属连接并移除下载组件；另一模式和系统浏览器保留。Firefox 本机配置按登录身份隔离，服务器卸载不清除客户端配置。
- Wisp 支持有界 TCP/UDP 复用；持续权限复核、CSRF、同源、流数与缓冲限制。正常 TCP 关闭后迟到的 DATA 不影响其余连接；重复使用原生 socket ID 有单独回归。

## 验证入口

- `make rewrite-test`：安装器 39 项（37 通过、2 项 root/VM 专属跳过）、MySQL driver race、全 Go race、96 个前端测试、TypeScript。
- `go vet ./...`。
- `tests/rewrite/test_server_browser.py`：真实服务端沙箱 Chromium、HTTP 网站、画面、输入、导航、标签、提示框、权限与清理。
- `tests/rewrite/test_browser_apps.py`：真实 UI 安装两种模式、原生 Firefox 地址栏访问、中文剪贴板输入、服务器 Wisp 出口、独立卸载、重启、双语和 390/320 手机宽度。外层浏览器拦截对测试站点的直接访问，站点实际收到 Firefox UA 的服务器请求。
- `internal/serverbrowser/apps_test.go`：默认关闭、安装/卸载、权限、过期修订、完整性与目录穿越、Wisp 鉴权/权限撤销、TCP 迟到报文与 ID 复用、UDP 回送。
- 独立引擎测试在隐藏 PATH 中系统 Chromium 的隔离实例执行，实际下载固定 headless shell 并访问受控站点。

最终冻结提交、包哈希、运行记录与验证限制见 [ACCEPTANCE.json](evidence/beta6-20261023/ACCEPTANCE.json)。截图只使用受控测试站点与模拟资源名称。

## 范围边界

真实运行环境为 Debian 13 amd64、非 root 面板进程、Chromium 154 客户端；独立引擎为固定 Chrome headless shell 156。这些是本轮浏览器功能验证，不扩展受管服务的主机平台支持矩阵。

ARM64 包交叉编译；未新增 ARM 运行、systemd VM 升级/回滚/重启、真实 DNS provider 或公开 CA 验收。beta4 的 VM 记录保留为历史。Firefox WASM 是上游实验性实现；面板应用与启动交接为中英双语，原生 Firefox 控件使用上游语言。

## 本轮截图

[中文应用页](../screenshots/browser-apps-beta6/zh-apps-desktop.png) · [英文应用页](../screenshots/browser-apps-beta6/en-apps-desktop.png) · [手机 390](../screenshots/browser-apps-beta6/zh-apps-390.png) · [手机 320](../screenshots/browser-apps-beta6/zh-apps-320.png) · [完整 Firefox WASM](../screenshots/browser-apps-beta6/firefox-wasm-desktop.png)。
