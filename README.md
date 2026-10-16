> **`refine` 分支重构交接**：Folio（折纸面板）是 AI 时代的 Linux 服务器可视化工具。Next.js + Astryx / Go / 原生 MCP 的新版源码和三阶段计划位于本分支。第一阶段已验收，第二阶段进行中，第三阶段尚未开始；当前不是正式发行版。先读 [本地开发交接](docs/rewrite/HANDOFF.md)、[接续 Codex prompt](docs/rewrite/CONTINUE-PROMPT.md) 和 [当前进度](docs/rewrite/PROGRESS.md)。新版构建与安装见 [scripts/rewrite/README.md](scripts/rewrite/README.md)；下方介绍与安装命令属于保留的旧实现。

<p align="center">
  <img src="./web/static/logo.png" width="120" alt="PowerLinux 3" />
</p>

# PowerLinux 3.0

#### 更现代、更统一的 Linux 控制面板

PowerLinux 3.0 是 PowerLinux 的第三代界面与体验升级版本。它延续了原有的安装、更新与卸载方式，在不改变使用习惯的前提下，重新梳理了首页、软件、网站、文件、日志与设置等核心页面，让整体视觉更统一，交互更顺手，也更适合浅色 / 深色 / 自定义背景图的使用场景。

## 3.0 版本亮点

- 面板默认标题统一为 `PowerLinux 3`
- 全局视觉语言重做，统一卡片、按钮、状态展示与层级关系
- 支持浅色 / 深色 / 跟随系统
- 支持背景图、自定义透明度与更柔和的主题色
- 首页状态、软件管理、网站管理、文件管理、日志与设置页全面优化
- 保留原有插件生态与常用命令，不改变安装和升级习惯
- 更适合长期使用的阅读体验与信息密度

## 截图

<img width="2884" height="1680" alt="WX20260505-135644@2x" src="https://github.com/user-attachments/assets/92e088ab-a8b9-40e7-9f36-0b36652cb04e" />




## 安装 / 更新 / 卸载


- **一键安装**

```bash
bash <(curl --insecure -fsSL https://cdn.jsdelivr.net/gh/AndyXeCM/PowerLinux@main/scripts/install.sh)
```

- **更新**

```bash
bash <(curl --insecure -fsSL https://cdn.jsdelivr.net/gh/AndyXeCM/PowerLinux@main/scripts/update.sh)
```

- **卸载**

```bash
wget --no-check-certificate -O uninstall.sh https://cdn.jsdelivr.net/gh/AndyXeCM/PowerLinux@main/scripts/uninstall.sh && bash uninstall.sh
```

## 文档入口

- [兼容性测试报告](./compatibility.md)
- [常用命令说明](./cmd.md)

## 生态说明

PowerLinux 3.0 继续兼容原有插件体系，常用能力包括但不限于：

- OpenResty
- PHP
- MySQL / MariaDB
- MongoDB
- PostgreSQL
- Redis
- Memcached
- phpMyAdmin
- Docker
- 其他第三方插件

欢迎继续通过 Issue 和 Pull Request 补充插件、修复问题和优化体验。

## 致谢

PowerLinux 3.0 基于 MWPanel 继续演进而来，由于需要被搜索的原因，现已脱离原项目fork分支，感谢原项目贡献！

## License

本项目采用 Apache 开源授权许可证，完整授权说明请见 [LICENSE](./LICENSE)。
