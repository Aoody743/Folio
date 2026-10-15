# Luna Max 批次证据（2026-10-15）

本目录只包含可随 Git 克隆的脱敏检查日志和摘要。代码由 GPT-6 Luna Max 子代理分工，主 Agent 审查、集成并运行全量检查；此前 Sol 批次见相邻目录。

- `checks.json`：每次构建、源码测试、已知失败和实际验收边界。
- `dev14-failed-arm64-build.txt`：保留 ARM64 Nlink 类型错误；该次不是完整发行包。
- dev.15：PHP/Supervisor 与界面功能；前端 57 项、类型检查及生产导出通过。
- dev.16：Web 迁移重载顺序修复；源码全量 race、双架构包和 amd64 私有前缀安装检查通过。真实 Ubuntu 迁移与浏览器记录位于上级目录。
- dev.17：数据库启动就绪等待及 health_failed 恢复初版；全量 race/vet、双架构和 amd64 安装检查通过。独立审查随后要求额外核对 systemd ActiveState/MainPID，不能将此包视为最终恢复验收版本。

- dev.18：补入真实 systemd ActiveState/MainPID 等值检查与负面测试；全量 race/vet、双架构及实际 amd64 包 smoke 通过；实际 VM 证据另见上级原生记录。普通云端预览已升级并通过健康与重复启动。

发行归档与临时 VM 不随 Git 提交；相应 CHECKSUMS 和逐版本源码 SHA 清单用于绑定身份。ARM64 交叉构建、普通前缀 smoke、源码模拟及实际服务验收分别记录，互不替代。包含早期历史证据的文件名不等于全部记录仅使用该版本。
