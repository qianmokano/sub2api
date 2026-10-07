# kano fork：统一认证与版本维护

首版基线为上游 `v0.2.11`（`96f4c115c9749078f90cbf210a01d39baf3f53b6`）。认证权威为 Casdoor；业务账号、余额、API Key 和订阅仍存储在 sub2api PostgreSQL。后续持续合并上游稳定版本，不将首版基线视为长期固定版本。

## 设置与认证流程

后台「注册与安全 → 通行证认证」独立维护地址、组织、应用和三个策略开关。页内认证使用 Casdoor session API，不需要 OAuth 客户端、秘密、scope、回调地址或 OIDC 开关。应用 ID 使用 `owner/name`；应用 owner 与用户 organization 是不同字段。

| 设置 | 默认 | 含义 |
| --- | --- | --- |
| `sso_enabled` | false | 页内 Casdoor 密码登录与 MFA |
| `sso_registration_enabled` | false | Casdoor 邮箱验证码注册；独立于本地注册、邮箱白名单和邀请码 |
| `sso_only_enabled` | false | 普通用户仅通过页内 Casdoor 认证；关闭其他认证及本地凭据管理 |

另外三个设置是 `sso_issuer_url`（HTTPS 根地址）、`sso_organization`（默认 `kano`）和 `sso_application`（默认 `admin/sub2api`）。应用应属于配置的用户组织。先启用页内认证并验证存量用户，再启用统一注册，最后启用仅统一登录。关闭页内认证前先关闭另两个开关。

管理员使用 `/login?local=1` 显示本地密码应急表单，后端验证角色、状态、人机验证和已有 TOTP；URL 参数本身不会授予权限。普通用户密码、找回密码及 MFA 管理进入通行证账户页。原业务会话及 refresh token 保持原有生命周期，已有本地 TOTP 保留登录和 step-up 验证。

页内登录不在浏览器建立 Casdoor 会话，首次进入另一个站点可能需要重新输入同一凭据。旧 `/auth/oauth/oidc/*` API 和 `/auth/oidc/callback` 页面已移除，返回 404；旧 OIDC pending 会话不能完成注册、绑定或登录。其他 OAuth Provider 继续使用各自的流程。

账户管理使用 `/login/kano` 建立正确组织的 Casdoor 浏览器会话，登录后进入通行证管理界面；找回密码使用 `/forget/sub2api`。不要将新浏览器直接送到 `/account`：无会话时该地址会落到 built-in 默认组织，无法认证 kano 普通用户。公开设置中的这两个地址按实际组织和应用生成。

后端提供 `/api/v1/auth/sso/captcha`、`/sso/password-login`、`/sso/mfa`、`/sso/register/send-code`、`/sso/register`。密码仅用于当次 HTTPS 代理，不落库、不记录日志。Casdoor Cookie 保存在 Redis，浏览器仅收到随机 MFA 挑战与可用验证方式；挑战 10 分钟有效、最多 10 次尝试，验证期间持有短期租约，成功后原子消费。Redis 故障拒绝认证。

身份键为 `(oidc, issuer, Casdoor user.Id)`，沿用原 OIDC `sub`。已绑定账号继续使用原业务数据；新身份要求已验证邮箱，不按相同邮箱自动接管旧账号。开户、身份绑定、初始余额和默认订阅在同一事务提交，重试不重复发放。Casdoor 注册成功但业务建号失败时，使用同一凭据重试登录；身份冲突需要管理员核对。平台额度快照保持上游 best-effort 行为。

验证码预检提交 `action`（`login`、`register-send-code` 或 `register`）和账号。后端只读取配置应用的验证码元数据，支持 Casdoor Default 图片和 Cloudflare Turnstile；浏览器仅得到图片或公开 site key。随后提交 `captcha: {challenge, answer}`。挑战在 Redis 中保存五分钟，绑定地址、应用、组织、动作及账号，验证前原子消费。配置或账号变化、过期、重放、Redis 故障均拒绝。本站 CAPTCHA 独立验证，两个 Turnstile widget 不共享 token。生产 CAPTCHA 策略不会因升级而改变。

迁移优先级为新数据库设置、旧数据库地址、新 YAML/env、旧 YAML/env、默认值。明确的 false 或空字符串具有优先权；旧客户端秘密和 OIDC enabled 不参与迁移。迁移 241 与首次启动事务保留旧行并写入完成标记，后续启动只读新设置。标记完成后缺失新地址或数据库错误会拒绝普通认证；管理员本地恢复入口仍可用。历史 `oidc` 身份、issuer、subject 和权益配置键保持不变，界面称为「通行证」。

## 发布与部署

1. 功能分支提交并推送，创建 PR；全部 CI 通过后合并到 fork `main`。
2. 从该 main 提交创建未使用的 `vX.Y.Z-N` 标签，`X.Y.Z` 是对应上游稳定版本，`N` 是从 1 开始的 fork 发布序号。例如 `v0.2.14-1`，同一上游版本后续为 `v0.2.14-2`；升级上游后从 `v0.2.15-1` 开始。标签触发 `Publish kano image`，再次测试后发布 `ghcr.io/qianmokano/sub2api:<标签>` 的 amd64/arm64 镜像和 release digest。禁止移动已发布标签，不发布 floating latest。旧 `vX.Y.Z-kano.N` 标签及镜像保留原名。
3. 生产 Compose 位于 `/opt/sub2api-deploy`，应用绑定 `127.0.0.1:8081`。先记录实际旧 image digest、容器挂载和健康状态；拉取新镜像成功后再进入停机窗口。
4. 停止应用写入，备份部署 `.env`、Compose、应用 `data`、PostgreSQL custom-format dump 与 Redis RDB/数据目录，设置仅管理员可读权限。数据库使用 `pg_dump`，不复制运行中数据库目录替代可恢复备份。Redis 使用 `SAVE` 完成快照再备份，按需停止 Redis 后复制目录。
5. 在同一 PostgreSQL 服务创建临时数据库，使用 `pg_restore --exit-on-error` 恢复备份并比对关键表计数，再删除临时数据库。恢复验证失败时保持旧镜像，不切换。
6. 仅更换 app image，保留 PostgreSQL/Redis 镜像、JWT/TOTP 密钥、端口、数据挂载和环境。执行 Compose 启动，检查 `/health`、日志、存量账号 ID/余额/Key/订阅、密码登录、MFA、注册、管理员恢复和关键 API 请求。
7. 依次启用三个开关并重复认证验收。记录实际镜像 digest、备份路径、恢复结果和用户数据对照，不在记录中包含凭据、Cookie 或 token。

本版执行设置迁移 241。切换前先在临时数据库及私有端口运行新镜像并验证迁移、健康和认证；回退必须同时恢复更新前数据库、Redis、应用文件与旧镜像。

备份在 VPS 上不等于完成异地备份；生产备份应定期复制到受控的外部存储，并演练恢复。运行配置与数据不提交 Git。

## 合并上游

1. `git fetch upstream --tags`；从 fork main 新建 `kano/upstream-vX.Y.Z`，检查目标稳定版本与安全记录。
2. `git merge vX.Y.Z`，保留 kano 定制提交，不 rebase 已发布历史，不强推 main。
3. 重点审查 Casdoor 适配、认证路由与策略、旧 OIDC 入口拒绝、本地 TOTP session、身份绑定事务、设置 DTO、认证 Store 和数据库迁移。
4. 运行完整 CI、统一认证回归与存量数据验收；PR 合并后发布对应上游版本的新 `vX.Y.Z-N` 镜像，按同一备份和回退流程部署。

定制主要集中在 `internal/pkg/casdoor`、`service/*sso*`、`handler/auth_sso.go`、`server/middleware/sso_guard.go` 与前端必要接线。fork 构建使用 `kano-container`；后端拒绝从上游下载替换程序，后台隐藏该更新入口。保留上游 release 工作流供以后合并，但其 prepare job 只在上游仓库执行；fork 使用独立 GHCR 工作流。
