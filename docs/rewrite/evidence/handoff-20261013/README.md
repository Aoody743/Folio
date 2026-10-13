# 可携带交接证据

这里保存普通用户源码检查、生产构建与实际 prefix 包检查的简短原始输出，以及仅含路径/摘要的源码 manifests。它们可随 `refine` 分支克隆，不包含 bootstrap token、测试账户密码、数据库、TLS/SSH私钥或 VM 磁盘。

- `dev10-source-manifest.json` 是实际已测 dev.10 的历史源码清单；不是当前分支源码。
- `handoff-source-manifest.json` 是当前交接源码的文件摘要；文档与生成资产不在该 source map 内。
- 新备份检查里的 `SKIP` 明确表示 root VM 条件未满足；不能用 `PASS` 汇总覆盖跳过。
- `vet.txt` 可以为空，表示 `go vet` 没有诊断；命令退出成功另由交接记录确认。
- 软件、服务、DNS、数据库和完整浏览器的历史实际检查仍见 `tests/rewrite/phase2/*-evidence.json`；未复制的云端大型原始日志路径是 provenance，不能在另一机器认为存在。

完整结果与限制见 [HANDOFF-VALIDATION](../../HANDOFF-VALIDATION.md)。
