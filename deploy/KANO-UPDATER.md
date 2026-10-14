# fork 后台容器升级

`kano-container` 只接受 `qianmokano/sub2api` 正式发布的 `vX.Y.Z-N`。每次升级拉取固定摘要镜像，保存全部数据挂载、完整 PostgreSQL 冷备及逻辑 dump、Redis 和原部署配置，再在独立网络中实际恢复并检查全部原业务字段。通行证、注册策略、初始权益、余额、Key、身份、订单和订阅必须保持。

主机上的 Go/systemd 服务独立于应用、PostgreSQL 和 Redis。应用只能访问受限的 Unix socket，没有 Docker socket 和备份目录权限。Nginx 提供 `/kano-updates/<任务编号>` 进度页；短期、单任务的 HttpOnly/Secure Cookie 限制访问，刷新或关闭页面不会取消任务。

## 首次安装

仅首次需要接线。发布附件包含 `kano-updater-linux-amd64.tar.gz`、`kano-updater-linux-arm64.tar.gz`、`checksums.txt` 和 `kano-update.json`。先下载对应架构附件及校验文件，用 `sha256sum -c checksums.txt --ignore-missing` 核对，再解压安装包。

审查 `config.example.json`。当前网关为 `/opt/sub2api-deploy`，应用 `sub2api`、PostgreSQL 18、Redis 8，应用绑定 `127.0.0.1:8081`；组 ID 为应用 1000、Nginx www-data 33。该 profile 保存在 `/etc/kano-updater/config.json`，只有 root 可修改。安装脚本按此部署激活；其他部署需调整配置及脚本的激活命令。

在解压目录运行 `sudo ./install.sh`。安装器保存原 Nginx 和 Compose 文件，添加单独的 socket overlay 和应用镜像 override；它增量添加独立进度路由及维护拦截，`nginx -t` 成功才 reload，保留原证书、HTTPS、端口、网络和代理配置。首次接线会用原镜像重新创建应用容器，挂载控制目录并启用客户端，不重建 PostgreSQL/Redis。

首次代码上线前，旧版应用没有提交按钮。安装服务后，从已通过完整发布检查的目标版本启动一次引导升级：

```sh
sudo kano-updater start --version vX.Y.Z-N --digest sha256:已发布的固定摘要
```

此命令同样走持久化任务、完整备份和实际恢复验证，只输出任务编号。上线后通过后台版本菜单使用「升级」和「查看升级进度」。无需生产制造额外空版本做试验。生产验收检查服务就绪、原管理员会话、已有 API Key 的只读接口及 HTTPS `/health`；完整点击升级、故障和中断恢复由隔离集成测试验收。

## 维护与恢复

镜像校验期间原服务继续运行。维护状态写入 `/var/lib/kano-updater-public/maintenance`，在主机重启后仍然保留，阻断应用新请求，独立进度路由继续可用。随后优雅停止应用，捕获停写基线和逻辑备份，停止数据服务，冷备全部真实挂载并验证文件 SHA256、模式和所有权。实际恢复使用全新 PostgreSQL 集群和独立挂载；内部网络禁止邮件、模型请求和后台探测的外部副作用。

执行状态、操作者、每步时间和恢复检查点保存在 `/var/lib/kano-updater/jobs.sqlite`，不依赖应用数据库。服务或主机重启后自动处理未完成任务：生产未迁移时启动旧版；迁移后且未开放流量时恢复旧镜像及全部更新前数据。开放流量的持久化提交点之后，不再自动恢复旧数据库，避免覆盖新业务写入。

启用主机更新客户端的应用在依赖注入前查询当前任务。升级仍活动时，HTTP、认证策略初始化和迁移正常启动，监控、聚合、保留期清理等后台作业延后，避免改动冻结数据；任务结束后按原顺序启动。确认任务活动后暂时断连，作业继续等待并重试。初次查询执行服务不可达时保持普通启动，避免可选更新功能阻断正常作业；主机仍必须完成全部数据验收才能开放升级流量。配置、环境和原有启用策略保持不变。

该约束也适用于独立文件中的 provider 和构造器自动清理，不能只检查 `wire.go`：上游余额探测、云用量同步、批量任务、缓存失效队列及内容审计自动清理均需经过同一协调器。HTTP 请求处理和安全策略的同步初始化保持可用；完整数据比较不能通过忽略账号扩展信息或保留期数据来绕过。

恢复失败时保持维护，任务显示需要管理员处理。受保护的 `/var/lib/kano-updater/diagnostics` 和任务备份目录保存详细诊断；`operation-<任务编号>.json` 记录具体失败原因，包括磁盘预检及检查点校验等非命令错误。诊断目录和文件仅 root 可读，页面不显示配置、凭据或原始命令输出。先修复实际故障，再停止执行服务并手动恢复：

```sh
sudo systemctl stop kano-updater
sudo kano-updater recover
sudo systemctl start kano-updater
```

若 `recover` 失败，不删除维护标志、不启动另一升级。完整备份保留在 `/opt/sub2api-backups/online-*`；临时隔离容器及恢复副本仅在不再挂载时清理，冷备、dump、globals 和旧镜像保留。定期把备份复制到主机之外并演练恢复。首版不提供已开放服务后的任意版本回退。

## 发布与兼容性

发布工作流在通过完整 CI 的 main 标签上生成机器可读清单：版本、双架构镜像摘要、源码提交、最低更新服务版本、迁移文件校验和及 `deploy/kano-upgrade-policy.json` 中的验收规则。服务自行检查正式发布、固定仓库、镜像标签/摘要/架构与源码版本，浏览器只能提交版本和预期摘要。

旧发布没有清单、主机 profile 不支持或迁移需要删除/转换旧业务数据时，后台拒绝在线安装并要求人工升级。新的表或列需在策略中声明并完成相应验收；旧字段、历史迁移校验值及未声明设置始终保留。原版构建继续使用原二进制更新逻辑，fork 的上游二进制下载、任意回退和重启入口保持禁用。

如需替换更新服务本身，先确认没有活动/需要处理的任务，再停止服务，校验安装包并替换 `/usr/local/bin/kano-updater`，保持配置、SQLite 和备份目录，随后启动服务。后台会根据清单的最低服务版本提示需先人工更新执行服务。
