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
