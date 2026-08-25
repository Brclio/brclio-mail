# 威胁模型

## 范围

本模型覆盖公开的 Web、SMTP/Submission、IMAP、单节点 SQLite、首选的 Linux/systemd 裸机部署、可选 Docker 主机、DNS/证书、smarthost 和备份。它不是渗透测试、形式化证明或合规认证。

需要保护的主要资产：邮件正文与附件、envelope/Bcc 元数据、账号与会话、应用密码、DKIM/relay/TLS 私钥、管理员归档、审计日志和可用性。

假设主机 operator 是受信任的最高权限角色；一旦 root、system manager、可选 Docker socket 或未加密数据库/备份被攻破，应用无法继续保证机密性或不可变性。普通管理员也被明确授权读取全部归档。

## 已实现的安全边界

| 风险 | 当前控制 |
| --- | --- |
| 开放中继 | 25 不提供 AUTH 且只接受有效本地收件人；外域投递只允许已认证的 465/587 会话 |
| 发件人伪造 | Submission 校验 envelope/header From 所有权；未认证入站不能使用本地域 From |
| 明文凭据 | 生产模式要求 TLS；默认公开 465/587/993，禁用明文 IMAP；最低 TLS 1.2 |
| 凭据库泄露 | 主密码使用带随机盐的 Argon2id；高熵应用密码以及 session/secret token 只保存不可逆 SHA-256 哈希 |
| 认证暴力尝试 | Web、SMTP Submission、IMAP 有按来源 IP/账号的有界进程内失败限制 |
| Web 会话与跨站请求 | Secure/HttpOnly/SameSite cookie、同源检查、JSON-only mutation、CSP/HSTS 等安全响应头 |
| 恶意邮件 HTML | 服务端和浏览器端进行 HTML 清理，浏览器阻止远程图片和危险元素；仍需持续安全测试 |
| 资源滥用 | 默认 25 MiB 消息上限、100 收件人/MIME 附件/邮箱目录上限、协议超时、SMTP/IMAP 全局与来源连接上限、Web 会话上限、用户逻辑配额、归档保守物理估算上限和 1 GiB 数据卷低水位 |
| 普通用户删除归档 | 用户 mailbox entry 与规范化消息分离；归档正文/附件访问要求原因并写审计事件 |
| 裸机服务越权 | systemd 固定无登录服务账号，只授予 `CAP_NET_BIND_SERVICE`；secret 内容不写入环境文件，root-only 源文件通过 `LoadCredential` 提供；主服务不能写 root 管理的本机备份库 |
| 容器越权 | Compose 使用非 root、只读根文件系统、drop all capabilities、no-new-privileges 与 PID 上限 |
| 数据库损坏 | WAL + FULL 同步、外键、最低 SQLite 版本门槛、doctor 与一致性备份 CLI |

## 重要残余风险

### 管理员和主机内部人员

管理员能够合法读取所有归档邮件，主机 operator 能绕过应用审计直接读取或修改 SQLite。审计表也和业务数据在同一个可写数据库中，不是防篡改外部账本。

措施：最小化管理员数量、独立 operator 账号、MFA/堡垒机（应用当前无内置 MFA）、集中只追加日志、定期复核归档访问，并在开户前向用户作显著披露。

### 垃圾邮件、恶意软件与邮件认证

当前入站不验证 SPF/DKIM/DMARC，不做信誉、DNSBL、灰名单、内容垃圾评分、附件杀毒或沙箱。一个公开 MX 会很快收到恶意流量。产品上限和开放中继保护不能替代反垃圾网关。

措施：Preview 仅限隔离/低风险试用；公网生产前在前置网关实现这些控制，或者等待原生实现。前置网关必须保留真实来源、正确限制只允许网关访问后端 25，并经过独立设计审查。

### 认证限速的局限

当前限速器保存在单进程内，重启会清零，不跨实例持久化，也不是完整连接/命令级 DoS 防护。分布式来源可以绕过单 IP 阈值。

措施：云防火墙、连接数限制、外部监控和基于日志的封禁；不能把 465/587/993 只按办公 IP 白名单时，应重点告警认证失败。不要把普通反向代理用于 SMTP/IMAP，除非它明确支持并正确配置这些协议。

### 数据静态机密性

SQLite 和内置备份没有应用层加密。数据库还含 DKIM 私钥与全部管理员归档。磁盘快照、systemd 服务凭据、备份服务、可选 Docker volume、崩溃转储和 root 用户都可能取得内容。

措施：宿主机全盘加密、加密异地备份、systemd credential 与 root-only 已发布本机备份、密钥分离、严格 root/可选 Docker 权限、禁用不必要快照、明确销毁流程。全盘加密不能保护已运行且已解锁的被攻破主机。

### 邮件传输不是端到端加密

客户端到本机使用 TLS，但邮件经过 smarthost 和远端 MTA 时是逐跳处理。Brclio Mail 没有内置 S/MIME/PGP。实验性直接投递没有 MTA-STS/DANE 强制，可能出现 TLS 降级或明文下一跳。

措施：保持 direct delivery 关闭，选择有明确 TLS/安全承诺的 smarthost；高敏感内容使用经过评估的端到端方案。

### 可用性和单点故障

单主机、单数据库、单数据卷是明确单点。磁盘满、SQLite 锁争用、主机故障、网络/DNS/PTR 错误或 25 端口封禁都可能停止收发。

措施：容量告警、外部端到端探测、频繁一致性备份、异地主机恢复演练和清晰 RTO/RPO。不要通过共享 NFS 或多副本绕过限制。

### 受信任账号与归档容量

用户 quota 衡量当前可见 mailbox entries；EXPUNGE 后逻辑 quota 会释放，但按产品保留规则，非草稿 raw MIME 仍进入全局管理员归档。当前没有独立的每用户永久归档预算或持久写入速率，因此恶意或失控的已认证账号可以反复 APPEND/发送再删除，消耗全局归档 cap 并影响其他用户收发。

措施：Preview 只给完全受信任的账号开户；监控每个账号行为、全局归档增长和 507/452 错误，出现异常立即停用账号。面向不受信任租户前必须实现可归因的 retained-bytes 配额、持久速率限制和管理员告警。

### 供应链与 Preview 变更

依赖、GitHub Actions、Go toolchain 和可选基础镜像都会变化；发布包有 SHA-256 清单但尚无签名、SBOM、可复现构建证明或独立安全审计。

措施：评审 lock file 和 CI 变更、运行 `govulncheck`、固定发布 digest、生成/验证 SBOM 和签名是后续路线图。不要把 `main` 的移动构建直接自动部署到关键环境。

## 部署者必须承担的控制

- 只从可信来源安装，检查 Git tag/commit、发布包 SHA-256 和可选镜像 digest；
- 修补宿主机、systemd、Go 依赖和可选 Docker/基础镜像；
- 使用云安全组和主机防火墙限制管理面，保护 root、unit/config/credential 和可选 Docker socket；
- 配置并定期核验 A/AAAA、MX、PTR、SPF、DKIM、DMARC、TLS-RPT；
- 使用强随机 setup token、每设备应用密码和加密备份；
- 选择可信 smarthost，并明确其数据处理责任；
- 给用户说明管理员保留和读取已删除邮件的能力；
- 制定事件响应、保留、删除、离职和执法请求流程；
- 在真实公网启用前完成独立安全评估。

安全问题请按[安全策略](../SECURITY.md)私下报告。
