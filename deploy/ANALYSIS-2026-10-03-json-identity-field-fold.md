# 统一身份字段检查与 JSON Unicode 折叠一致

日期：2026-10-03，基线 fork main `5e20b67`。两站账户面板统一最终验收的补充；不更改运行账号、通行证或业务设置。

## 依据与修改

- 标准库 `encoding/json/fold.go` 说明字段折叠与 EqualFold 一致，包括 Unicode SimpleFold；简单 ToLower 不具备相同语义。
- 实际 HTTP 回归复现：`uſername: null` 与通知设置的混合请求仍返回 200，管理员的 `paſſword: null` 混合请求也漏过检查。
- 参考现有个人原始 JSON 检查、管理员身份 guard、httputil 请求体工具三处模式，将公共字段出现判断集中为 `httputil.HasJSONField`，使用 `strings.EqualFold`。个人及后台共用，不重复实现比较规则。
- null 和字段别名仍按出现处理；管理员、本地模式及业务请求的授权规则不变。未修改数据库结构或部署配置。
- 已发布标签不重用或移动；`.6` 不切换生产，完成本补充后使用新的未用标签。当前线上仍为 `.5`，业务指纹及测试资料已还原。

## 验证与下一步

新增公共工具测试与实际 JSON 解码结果对照，覆盖 ASCII、Unicode long s／Kelvin、null、false、大小写重复字段、空输入及不应匹配的 Unicode 字符；扩充个人及管理员 HTTP 回归。完整 CI、构建和 PostgreSQL 集成测试通过后发布新镜像，按已授权的完整备份、实际恢复、隔离验证及先网关后商城顺序上线，再执行两站实际资料更新与恢复验收。
