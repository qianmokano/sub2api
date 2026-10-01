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
