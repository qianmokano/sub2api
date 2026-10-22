# 2026-10-22 上游 v0.2.17 同步与安全修复分析

## 背景

- fork main（4b3ae422，v0.2.16-7 基线）上的 Security Scan 自 10-19 后开始失败，与具体改动无关：
  - backend-security：govulncheck 新命中 GO-2026-6809（crypto/x509 信任链问题），Go 1.27.2 受影响、1.27.3 修复。
  - frontend-security：follow-redirects 新公开 GHSA-q3jh-wrc6-jgq8 / GHSA-8r9p-f939-6h3c 两个 high，锁定版本 1.16.0。
- 上游在 v0.2.17 标签之后恰好合入修复：cacc80b86（go.mod → 1.27.3、follow-redirects override → 1.16.1、上游 security-scan 断言）、59100d324（根 Dockerfile → golang:1.27.3-alpine）。

## 本次分支内容（kano/upstream-v0.2.17）

1. `merge upstream stable tag v0.2.17`：v0.2.16..v0.2.17 共 48 个提交（网关 Responses created_at、EasyPay/支付宝/Stripe 支付修复、Gemini 4 Argon 与 Vertex 错误策略、GPT-5.5 retirement、账号过滤 revert 等）。无冲突自动合并。
2. cherry-pick `-x` cacc80b86、59100d324：均干净应用。
3. `chore(security): finish Go 1.27.3 alignment`：fork 自有文件补齐——backend-ci.yml ×2、release.yml ×1 的 `grep go1.27.2` 断言；deploy/Dockerfile、backend/Dockerfile（上游未改、无 CI 引用）镜像对齐 1.27.3。

## 语义冲突核对

- 两侧共同修改的文件仅 `backend/internal/config/config.go`、`config_test.go`、`deploy/config.example.yaml`（上游新增 gateway.min_output_tokens，fork 改动在别处），合并结果人工核对无相互覆盖。
- `backend/internal/service/sso_auth.go`、`internal/pkg/casdoor` 等 fork 专有认证文件零改动。
- v0.2.17 无数据库迁移（`git diff v0.2.16..v0.2.17 -- backend/migrations` 为空），无需审查 `deploy/kano-upgrade-policy.json`。

## 本地验证

- `pnpm --dir frontend install --frozen-lockfile`（lockfile 一致）、`make test-frontend`（637 项，含 zh/en 键一致性）、`pnpm --dir frontend run build`。
- `make -C backend test-unit`（GOTOOLCHAIN=auto 自动下载 go1.27.3）、`make -C backend build`。

## 后续

- PR 合并后，PR #18（通行证域名白名单错误提示）需把新 main 合入分支重跑检查，再合并。
- 打标签 `v0.2.17-1`（上游 0.2.17 的首个 kano 发布），走完整发布验收。
- 限制：上游 backend/Dockerfile、deploy/Dockerfile 上游本身仍标 1.27.2，fork 已自行对齐；后续上游若再动这些文件注意不要回退。
