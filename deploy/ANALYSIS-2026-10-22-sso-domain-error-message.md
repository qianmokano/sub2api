# 2026-10-22 通行证注册域名白名单错误提示分析

## 现象

casdoor-kano 新增组织级 `EmailDomains` 注册白名单（e0a547e1，5a541334 同时覆盖发送验证码）后，用白名单外邮箱在 sub2api 内注册 kano 通行证时，前端提示"kano 通行证暂时不可用，请稍后重试"（`SSO_PROVIDER_UNAVAILABLE`），误导用户以为是临时故障。

## 根因

- casdoor 对 `/api/signup` 与 `/api/send-verification-code`（method=signup）返回
  `{"status":"error","msg":"The email domain is not allowed to sign up"}`。sub2api 后端客户端不带
  Accept-Language，casdoor 默认按英文返回，消息稳定。
- `backend/internal/pkg/casdoor/client.go` 的 `Register`/`SendCode` 只识别 captcha / code /
  already exists / only send one code 等消息，白名单拒绝落入兜底 `ErrUnavailable`。
- `backend/internal/service/sso_auth.go` 的 `ssoProviderError` 把未知上游错误统一映射为
  `SSO_PROVIDER_UNAVAILABLE`（503），前端按 `auth.errors.SSO_PROVIDER_UNAVAILABLE` 渲染"暂时不可用"。

## 修正（本分支）

1. casdoor 客户端新增哨兵 `ErrEmailDomain`，`Register` 与 `SendCode` 在消息包含 `email domain` 时返回它。
2. `ssoProviderError` 映射为 `Forbidden("SSO_EMAIL_DOMAIN_NOT_ALLOWED", "This email domain is not allowed to sign up")`
   ——与 `SSO_IDENTITY_INVALID` 同为策略性 403，前端 axios 拦截器只对 401/404 有全局处理，403 安全。
3. 前端 `auth.errors` 增加 `SSO_EMAIL_DOMAIN_NOT_ALLOWED`：
   zh "该邮箱域名不允许注册 kano 通行证。"，en 对应英文（措辞与 casdoor 中文文案一致）。
4. 单测覆盖：客户端两个入口的消息映射、服务层错误码映射表。

## 验证与后续

- 本地：casdoor 包测试、`-tags=unit` SSO 服务测试、`make -C backend test-unit`、`make test-frontend`
  （含 zh/en 键一致性）、前端构建均通过。
- 后续：PR 合并后随常规发布上线；发验证码入口同样会得到正确提示（casdoor 5a541334 已在发送侧拦截）。
- 限制：匹配依赖 casdoor 英文消息子串（与其余错误识别方式一致）；若上游改文案需同步。
