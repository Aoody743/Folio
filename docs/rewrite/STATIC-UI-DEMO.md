# 静态交互演示

演示复用正式 Next 静态导出的全部页面，再单独注入 `frontend/demo/runtime.js` 与明确的演示数据。正式产品不导入演示脚本，不改变原来的认证、授权或业务接口。

构建前先生成当前前端及嵌入服务二进制，然后运行：

```sh
python3 scripts/rewrite/build-demo.py --binary /absolute/path/to/folio --output /absolute/path/to/site/dist
python3 -m http.server 4178 --bind 127.0.0.1 --directory /absolute/path/to/site/dist
FOLIO_DEMO_URL=http://127.0.0.1:4178 python3 tests/rewrite/test_static_demo_browser.py
```

构建依赖 Python Playwright、Pillow，字体再生成依赖 `fonttools[woff]`。构建器从隔离服务读取公开能力描述与软件目录，复用截图展示数据，不导出真实主机数据、管理员密码或登录令牌。输出目录必须专用于本演示，因为重新构建会替换它。

支持新建网站及本地任务进度、软件变更计划、连接配置、文件编辑/小文件上传/下载、计划任务、告警、便签、假令牌以及主题、字体、语言和页面导航。修改保存在当前浏览器，可通过「演示数据 → 重置演示」恢复；请勿填写真实凭据或上传敏感内容。

终端只有几个模拟命令，面板浏览器使用样例画面，Firefox 页面为明确标注的独立窗口外观演示。不提供真实服务器执行、网络代理、WASM 引擎或外部数据库连接；未实现的高级动作返回明确的演示提示，不宣称成功。

本轮验证了真实界面的登录与缺字回退、新建网站确认及任务、刷新保留、文件编辑保存、模拟终端、浏览器标签与地址栏、12 个入口的桌面和两种手机宽度。所有模拟 API 都由页面内处理，浏览器没有发起真实服务 API 请求。部署保留 Sites 的默认私有访问范围。

## 缺字符号修复

Astryx 的 FieldLabel 用 U+2219（`∙`）分隔字段名称和必填/可选信息。Inter 与 Noto Sans SC / Noto Serif SC 的源字体缺少此字形；在截图环境缺乏合适系统回退时显示成方框。中文字体分片没有删除这个字形，问题来自原字体覆盖和系统回退。

`Folio Symbols` 从已有开放许可 Source Sans 3 字体提取该字形，仅 1,056 字节，并作为三套预设的本地回退。它不替换必填提示、不修改认证行为，也不依赖外部字体服务。使用 `scripts/rewrite/subset-symbols.py` 可再生成；浏览器回归通过 CDP 检查实际使用的自带字体与渲染字形。
