# kano 页内统一认证实施记录

日期：2026-10-01（Asia/Shanghai）。生产基线：上游 v0.2.11，提交 96f4c115c9749078f90cbf210a01d39baf3f53b6。

## 结论与依据

- fork main 固定在生产版本；新 fork 导入的较新上游 main 保留为 upstream-initial-main。工作区为 sub2api-kano，不修改原 sub2api 工作区。
- Casdoor 4.13.0 JWT 的 sub 是 user.Id，与 get-account 返回的 id 一致。页内认证使用现有 (oidc, issuer, subject) 身份键，保留已经预绑定的业务账号。
- 上游 OIDC、邮箱密码登录、TOTP 登录、设置读写与前端认证 Store 是本次实现参考；统一身份建号必须在签发 token 前完成全部验证。
- 生产 Compose 位于 /opt/sub2api-deploy，应用监听 127.0.0.1:8081；PostgreSQL 的实际 data_directory 是 /var/lib/postgresql/data，由 postgres_data 挂载。Redis 与应用分别使用 redis_data、data。
- 仅统一登录需要后端策略，不能仅隐藏表单；管理员本地认证保留角色校验与 TOTP。

## 范围与下一步

实现页内登录、验证码注册、Casdoor MFA、后端认证策略、前端与后台设置；补充测试、固定版本镜像发布、备份及回退文档。密码和认证 Cookie 不写入本文或日志。

后续升级从 fork main 创建 kano/upstream-vX.Y.Z，merge 上游稳定标签，完成统一认证回归后发布 vX.Y.Z-kano.N。禁止强推发布历史或直接使用上游一键更新覆盖定制。

## 验证与 CI 修正

- Casdoor HTTP 适配测试覆盖率 94.2%；新增服务文件合计覆盖 188/223 个语句（84.3%）。挑战存储验证过期、错码重试、Cookie 轮换、租约与并发单次消费；处理器验证两层 MFA 完成前无 token、管理员恢复及 OIDC 兜底。
- 前端类型检查、24 个测试文件共 320 项测试、lint 和生产构建通过；Go embed 构建通过。本机 Docker 未运行，PostgreSQL 真实并发开户测试必须由 CI 实际执行。
- 首轮 CI 找到设置接口旧断言缺少新增 SSO 字段及四处 errcheck 问题，已修正。安全扫描发现基线 Axios 1.18.1 的新增高危公告，升级为 1.20.0；pnpm 9 生成的锁文件仅修改 Axios。安全例外检查及升级后前端测试与构建通过。
- 生产只读核对：128 个业务用户、139 个 API Key、124 个 Casdoor OIDC 绑定；管理员当前未启用本地 TOTP，Turnstile 开启。线上尚未切换。停机备份时再次记录最终基线，部署后对照账号与业务数据。
- 部署前发现 Casdoor 应用 ID 的 owner 与用户 organization 被错误要求相同；生产应用为 `admin/sub2api`、用户组织为 `kano`。已拆开校验并修正默认值与契约测试，保留服务端 get-account 的用户组织校验。首个 `v0.2.11-kano.1` 标签发布流程已取消，未部署；修正通过 CI 后使用新标签发布，不移动旧标签。
