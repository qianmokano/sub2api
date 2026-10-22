# 2026-10-22 升级前自动清理旧备份

## 背景

v0.2.17-1 两次立即升级（`up-0fd5c86b…`、`up-7a9bd4c1…`）在 preflight 磁盘预算处被拒
（`insufficient disk space for a complete backup and isolated restore`）：39G 盘仅剩 8.3G，
预算约 14–15G。主因是历史升级留下的展开完整备份（10-07 一份 3.6G、10-16 两份各 3.5G）与
19 个镜像。当日已按管理员指示手动删除旧备份与旧镜像（可用空间恢复到 23G），同时决定把
"升级前清理旧备份"做进执行器，作为下个版本的内容。

## 设计

- 时机：`Engine.Execute` 进入 Preflight、核对运行版本之后、`storageBudget` 之前。这样预算
  计算能反映可回收空间；预约任务也在 04:00 窗口内执行同一逻辑。
- 范围（全部满足才删）：
  1. `/opt/sub2api-backups` 下的一级目录，命名严格匹配 `online-<8位日期>T<6位时间>Z-<32位任务哈希>`；
  2. 由目录名还原任务号 `up-<哈希>` 后能在任务库中查到，且状态为非活动
     （succeeded / failed / failed_rolled_back / cancelled / missed）；
  3. 不是当前任务自身的目录（防同任务重入误删自己的检查点目录）；
  4. 未被任何容器（含已停止容器）挂载，挂载源列表来自 `docker ps -aq` + 逐个 inspect。
- 不动：`.tar.gz` 归档、`upstream-*`/`passport-*`/裸时间戳等手工目录、任务库查不到的目录、
  任何仍处活动状态任务的目录。
- 失败语义：备份目录不存在视为无事发生；docker 探测失败让预检以
  "Superseded backup cleanup failed" 失败（fail-safe，宁可拒绝升级也不盲删）；单个目录删除
  失败仅跳过，磁盘预算随后自然裁决。
- 可观测性：删除清单写入 `UpgradeState.PrunedBackups`（随检查点与 snapshot.json 持久化），
  有删除时进度消息更新为 "Pruned N superseded backup(s)…"。
- 不加配置开关：行为即产品要求；文档（KANO-UPDATER.md）已同步说明，并提醒需留存的备份
  应在下一次升级前复制到主机之外。

## 实现与测试

- `internal/kanoagent/files.go`：`pruneSupersededBackups`、`mountedPaths`、
  `supersededBackupPattern`。
- `internal/kanoagent/runtime.go`：`UpgradeState.PrunedBackups`（JSON 向后兼容，omitempty）。
- `internal/kanoagent/engine.go`：Execute 接入，失败原因链新增清理失败分支。
- `internal/kanoagent/backup_prune_test.go`：仅删终结任务的受管目录（保留当前任务、未知
  任务、手工命名、归档文件）；活动任务目录保留（经任务库单活动索引说明，用受控 SQL 构造
  防御场景）；挂载目录保留；备份目录缺失、docker 不可用等边界。新增函数覆盖 96.3% / 91.7%，
  包覆盖 80.4%；`go test -race -tags=unit ./internal/kanoagent ./internal/kanoupdate
  ./cmd/kano-updater` 全部通过。

## 后续

- 随下个发布（预计 v0.2.17-2）交付；生产执行器需按流程先更新到包含本功能的版本，再执行
  后续应用升级。
- 限制：只处理执行器自管的 `online-*` 展开目录，不清理归档 tar.gz 或镜像（镜像保留策略
  另行决定）；跨主机留存仍需人工完成。
