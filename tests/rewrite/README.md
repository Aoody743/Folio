# Independent release verification

These tests run the real compiled Go service, exported Next.js/Astryx UI and official-SDK MCP protocols. No fixtures are returned by the runtime; test accounts and data are private, disposable state. Credentials are generated at runtime, held in memory and never printed. Browser traces are disabled because traces can capture passwords and access tokens.

```bash
POWERLINUX_TEST_BINARY=/absolute/path/to/powerlinux python3 tests/rewrite/test_phase1.py
POWERLINUX_TEST_BINARY=/absolute/path/to/powerlinux python3 tests/rewrite/test_browser.py
```

The first script verifies bootstrap, cookie/CSRF/logout, human/MCP note collaboration and stale revisions, scoped resources, token revocation, both MCP transports, note size/lifecycle, TOTP and replay denial, non-global rate limiting, real durable file tasks and a preliminary HTTP/RSS measurement. The benchmark compiles `benchmark_http.go` with `GO_BIN` (or the pinned cloud Go path); optionally provide an already compiled helper as `POWERLINUX_TEST_BENCHMARK`. It requires an isolated writable data directory and launches an ordinary-user service itself.

The browser script requires Python Playwright and actual Chromium (`POWERLINUX_CHROMIUM`, default `/usr/bin/chromium`). It verifies five navigation entries, four overview cards, five themes, 320/390/768/1440px layouts, theme persistence, notes via UI/MCP, genuine concurrent writes, merge, offline draft/retry, history, Markdown XSS, real API token source labels and keyboard dialogs. It saves screenshots to `POWERLINUX_QA_ARTIFACTS`, default `/workspace/powerlinux/qa-artifacts`, outside the checkout.

The preliminary performance measurement is from the cloud workspace, not a controlled performance certification or the fixed 2vCPU/2GiB production benchmark. It measures end-to-end localhost HTTP with a Go keep-alive client,20concurrent requests and200 measured samples (100warmups), rather than instrumented server-only timing. The p95<100ms and API RSS≤100MiB checks remain enforced. Root/helper fixtures are created outside checkout. It never proves all plugin or phase-two/three capabilities.

`vm/start-vm.sh` boots a **real QEMU VM** with its own verified Debian kernel, initramfs, systemd PID1 and block disk. Docker is used only to prepare the disposable disk. A running container is not accepted as VM evidence. Actual dev.3 VM installation/repeat/restart/uninstall passed; different-version upgrade/reboot remain separate future checks, rather than being inferred from prefix installer or unit tests; see `vm/EVIDENCE.md` for exact measured results and limitations.

## Benchmark measurement correction

An initial Python stopwatch included constructing `APIClient` for every request. In Python3.12, the default urllib HTTPSHandler loads a TLS CA context during opener construction even when the target is HTTP. Independent200/20 profiling measured constructorp95≈1864ms; those initial request p95s were therefore not pureHTTP evidence. Per-worker urllib reuse still showed its own scheduling/socket baseline (health165ms, businessstatus115ms), so the independent Go client measures actual requests with a shared keep-alive transport. No request count, concurrency or goal was relaxed.

Actual dev.3 full fourteen-case black-box run passed: statusHTTPp50=8.77ms/p95=38.14ms, healthp95=4.56ms, API RSS100,752kB (~98.39MiB). This is a cloud workspace baseline, not an assertion of a controlled2vCPU/2GiB host or a multi-process privileged Agent budget.

## Current information UI and polish regressions

`test_information_browser.py` covers the current three-area overview, five-item
bottom navigation, hash routes, list pagination, drafts and the Chinese/English ×
three-font × five-palette matrix over five desktop/mobile sizes. Website and app
collections are explicit display fixtures; files use a real isolated server.

`test_ui_polish_browser.py` covers semantic mobile website/schedule columns,
empty recycle-bin wording, translated upload selection without submitting a
file, unavailable/forbidden certificate states, advanced-tool classification and
pagination, terminal guidance and default font transfer size. Authentication and
files are real; the site rows, schedule and certificate failures are display
fixtures. It executes no host/software/site mutations.

```sh
POWERLINUX_TEST_BINARY=/absolute/path/to/folio python3 tests/rewrite/test_ui_polish_browser.py
```

Both scripts use `POWERLINUX_CHROMIUM` for Chromium and
`POWERLINUX_QA_ARTIFACTS` for screenshots. Keep those artifacts outside the
checkout until deliberately selecting documented examples.

### 创建与监控界面回归

`test_creation_monitor_browser.py` 使用 `POWERLINUX_TEST_BINARY` 启动隔离服务，验证网站/应用默认标识、手动修改、草稿、确认和失败重试幂等、监控读数与时间范围，以及实际创建 MCP 令牌后的权限和有效期。网站提交被拦截，不写入主机。展示夹具与实际接口验证分别标注，截图输出到 `POWERLINUX_QA_ARTIFACTS`。

### 搜索、文件队列、告警与保留策略

`test_resource_tools_browser.py`：真实隔离服务验证两文件上传、第二项故障暂停/继续、完成项不重复上传、排序路由与刷新、多选预检及真实批量复制；搜索资源仅用明确的展示夹具。手机检查过滤控件无重叠、列表至少四项、无页面水平溢出，并覆盖从告警切到文件的导航。

`test_safety_tools_browser.py`：实际保存/更新默认停用的告警和资源绑定保留策略。清理预览使用展示夹具，执行提交拦截为失败，核对确认和目标参数，未删除主机备份。

两者均使用 `POWERLINUX_TEST_BINARY`，截图写入 `POWERLINUX_QA_ARTIFACTS`。Go 的 `internal/systemops/alerts_test.go` 与 `retention_test.go` 验证真实元数据、权限重检、任务保留和受控派发源，不向外部发送通知。

### 可复现图库

`capture_panel_gallery.py` 输出 33 张桌面与 8 张手机实际视口截图；列表/指标等使用只读展示数据，认证和软件目录来自实际隔离服务。夹具遵守网站/文件分页、过滤与归档标记。

`capture_browser_gallery.py` 会在临时服务内按需安装两种浏览器组件，再实际访问 MDN 和本机演示站点；它可能下载固定版本组件。输出模式管理、Chromium 画面与 Firefox WASM 页面截图，并检查 Firefox 页面完成状态。

两脚本使用相同的 `POWERLINUX_TEST_BINARY` / `POWERLINUX_CHROMIUM` 环境参数，可通过 `POWERLINUX_QA_ARTIFACTS` 指定输出目录；默认输出到专用开发图库，保留历史发行截图。截图不是实际主机业务验收的替代品。
