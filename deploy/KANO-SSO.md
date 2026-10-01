# kano fork：统一认证与版本维护

首版基线为上游 `v0.2.11`（`96f4c115c9749078f90cbf210a01d39baf3f53b6`）。认证权威为 Casdoor；业务账号、余额、API Key 和订阅仍存储在 sub2api PostgreSQL。后续持续合并上游稳定版本，不将首版基线视为长期固定版本。

## 设置与认证流程

在后台 OIDC 设置中保留现有 issuer、客户端和回调配置。页内代理补充 `sso_organization=kano`、`sso_application=admin/sub2api`，不需要在浏览器提供客户端秘密。应用 ID 使用 Casdoor 的 `owner/name`；应用 owner 与用户 organization 是不同字段，不要求相同。

| 设置 | 默认 | 含义 |
| --- | --- | --- |
| `sso_enabled` | false | 页内 Casdoor 密码登录与 MFA；OIDC 使用同一身份解析和本地 TOTP 门禁 |
| `sso_registration_enabled` | false | Casdoor 邮箱验证码注册；独立于本地注册、邮箱白名单和邀请码 |
| `sso_only_enabled` | false | 普通用户仅通过 Casdoor 密码或 OIDC 登录；关闭其他认证及本地凭据管理 |

应用应配置为该用户组织的应用，issuer 必须为已启用的 HTTPS OIDC 地址。先启用页内认证并验证存量用户，再启用统一注册，最后启用仅统一登录。关闭页内认证前先关闭另两个开关。

管理员使用 `/login?local=1` 显示本地密码应急表单，后端验证角色、状态、人机验证和已有 TOTP；URL 参数本身不会授予权限。普通用户密码、找回密码及 MFA 管理进入通行证账户页。原业务会话及 refresh token 保持原有生命周期，已有本地 TOTP 保留登录和 step-up 验证。

页内登录不在浏览器建立 Casdoor 会话，首次进入另一个站点可能需要重新输入同一凭据。OIDC 跳转入口保留为兜底。

账户管理使用 `/login/kano` 建立正确组织的 Casdoor 浏览器会话，登录后进入通行证管理界面；找回密码使用 `/forget/sub2api`。不要将新浏览器直接送到 `/account`：无会话时该地址会落到 built-in 默认组织，无法认证 kano 普通用户。公开设置中的这两个地址按实际组织和应用生成。

后端新增 `/api/v1/auth/sso/password-login`、`/sso/mfa`、`/sso/register/send-code`、`/sso/register`。密码仅用于当次 HTTPS 代理，不落库、不记录日志。Casdoor Cookie 保存在 Redis，浏览器仅收到随机 MFA 挑战与可用验证方式；挑战 10 分钟有效、最多 10 次尝试，验证期间持有短期租约，成功后原子消费。Redis 故障拒绝认证。

身份键为 `(oidc, issuer, Casdoor user.Id)`，沿用原 OIDC `sub`。已绑定账号继续使用原业务数据；新身份要求已验证邮箱，不按相同邮箱自动接管旧账号。开户、身份绑定、初始余额和默认订阅在同一事务提交，重试不重复发放。Casdoor 注册成功但业务建号失败时，使用同一凭据重试登录；身份冲突需要管理员核对。平台额度快照保持上游 best-effort 行为。

## 发布与部署

1. 功能分支提交并推送，创建 PR；全部 CI 通过后合并到 fork `main`。
2. 从该 main 提交创建未使用的 `vX.Y.Z-kano.N` 标签。标签触发 `Publish kano image`，再次测试后发布 `ghcr.io/qianmokano/sub2api:<标签>` 的 amd64/arm64 镜像和 release digest。禁止移动已发布标签，不发布 floating latest。
3. 生产 Compose 位于 `/opt/sub2api-deploy`，应用绑定 `127.0.0.1:8081`。先记录实际旧 image digest、容器挂载和健康状态；拉取新镜像成功后再进入停机窗口。
4. 停止应用写入，备份部署 `.env`、Compose、应用 `data`、PostgreSQL custom-format dump 与 Redis RDB/数据目录，设置仅管理员可读权限。数据库使用 `pg_dump`，不复制运行中数据库目录替代可恢复备份。Redis 使用 `SAVE` 完成快照再备份，按需停止 Redis 后复制目录。
5. 在同一 PostgreSQL 服务创建临时数据库，使用 `pg_restore --exit-on-error` 恢复备份并比对关键表计数，再删除临时数据库。恢复验证失败时保持旧镜像，不切换。
6. 仅更换 app image，保留 PostgreSQL/Redis 镜像、JWT/TOTP 密钥、端口、数据挂载和环境。执行 Compose 启动，检查 `/health`、日志、存量账号 ID/余额/Key/订阅、密码登录、MFA、注册、管理员恢复和关键 API 请求。
7. 依次启用三个开关并重复认证验收。记录实际镜像 digest、备份路径、恢复结果和用户数据对照，不在记录中包含凭据、Cookie 或 token。

首版没有新增数据库迁移。故障时可先关闭统一认证策略，重新使用已记录的旧镜像。若未来版本执行数据库迁移，回退必须同时恢复更新前数据库、Redis 和应用数据备份，避免旧程序读取不兼容数据。

备份在 VPS 上不等于完成异地备份；生产备份应定期复制到受控的外部存储，并演练恢复。运行配置与数据不提交 Git。

## 合并上游

1. `git fetch upstream --tags`；从 fork main 新建 `kano/upstream-vX.Y.Z`，检查目标稳定版本与安全记录。
2. `git merge vX.Y.Z`，保留 kano 定制提交，不 rebase 已发布历史，不强推 main。
3. 重点审查 Casdoor 适配、认证路由与策略、OIDC 回调、本地 TOTP session、身份绑定事务、设置 DTO、认证 Store 和数据库迁移。
4. 运行完整 CI、统一认证回归与存量数据验收；PR 合并后发布对应上游版本的新 `vX.Y.Z-kano.N` 镜像，按同一备份和回退流程部署。

定制主要集中在 `internal/pkg/casdoor`、`service/*sso*`、`handler/auth_sso.go`、`server/middleware/sso_guard.go` 与前端必要接线。fork 构建使用 `kano-container`；后端拒绝从上游下载替换程序，后台隐藏该更新入口。保留上游 release 工作流供以后合并，但其 prepare job 只在上游仓库执行；fork 使用独立 GHCR 工作流。
