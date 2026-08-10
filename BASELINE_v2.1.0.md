# codex-redteam-mode v2.1.0 基线

- 源目录：`E:\cli\codex-redteam-mode\codex-redteam-mode-main`
- 备份目录：`E:\cli\codex-redteam-agent\codex-redteam-mode-v2.1.0-backup`
- 备份清单：`E:\cli\codex-redteam-agent\BACKUP_MANIFEST_v2.1.0.json`
- 文件数：62（排除 `.git`、`.pytest_cache`、`__pycache__`）
- 聚合 SHA-256：`1180f54761b3a03e2b1854aaece170791ceaad9586750bfff8457e7a928888d9`

## 基线验证

```text
173 passed, 1 skipped in 39.50s
Validation PASSED
runtime: 8 typed workflows valid
runtime: MCP server self-test passed
history injection: 6 role pairs valid (websockets 16.0)
```

本备份仅用于后续独立 Agent 方案迁移和回归对照；方案一的改造继续在原项目目录进行。
