# 架构

## 定位与部署单元

Brclio Mail Preview 是一个模块化单体：一个 Go 二进制同时提供 Web/API、SMTP、Submission、IMAP 和队列 worker；所有持久状态进入同一台主机的 SQLite 数据库。首选部署由 systemd 以受限服务账号启动一个应用副本，Docker Compose 是可选封装。

```text
Internet MTA -- TCP 25 --------> SMTP inbound ----+
Mail clients -- TCP 465/587 ---> Submission ------+--> service --> SQLite WAL
Mail clients -- TCP 993 -------> IMAPS -----------+       |          |
Browser ------ TCP 443 --------> Web/API ---------+       +--> queue--+
                          TCP 80 ACME/redirect             |
                                                         +--> smarthost (recommended)
                                                         +--> remote MX (experimental)
```

systemd 模板直接绑定公网 `80/443/25/465/587/993`，unit 只授予低端口绑定能力；可选 Compose 则把这些主机端口映射到容器 `8080/8443/2525/2465/2587/2993`。明文 IMAP listener 默认禁用，`BRCLIO_IMAP_ADDR` 只允许在开发模式绑定回环地址且不得启用 STARTTLS；生产客户端必须使用 993 隐式 TLS。生产 Submission 认证必须在 TLS 内进行。

## 组件

### Web/API

- 首次初始化、会话登录、网页邮箱与附件；
- 域名、用户、容量、别名和应用专用密码；
- DNS TXT 域名所有权检查；域名未变为 `verified` 前不能公网收信或外发；
- 管理员队列、归档、统计和审计查看；
- JSON 请求限制、同源检查、Secure/HttpOnly/SameSite cookie、CSP/HSTS 等响应头；
- Web 登录和 setup 具有进程内失败次数限制。

### SMTP

- 25 端口只接受送往已配置本地域和有效收件人的邮件，不暴露 AUTH；
- 465/587 要求认证后才允许外域收件人；
- 已认证用户的 envelope From 与消息 `From` 必须属于该用户或其别名；
- 未认证入站不能伪造本地域 `From`；
- 单封默认上限 25 MiB、单事务最多 100 个收件人、读写超时 5 分钟；
- 465 与 587 共用按来源 IP/账号的有界进程内认证失败限制器。

这些规则防止把端口 25 当成开放中继，但不等于完整反滥用系统。入站尚无 SPF/DKIM/DMARC 验证、灰名单、垃圾评分或病毒扫描。

### IMAP

- 默认只公开 993 隐式 TLS；
- 支持标准邮箱、flags、移动、删除/EXPUNGE 和应用专用密码；
- 认证失败按来源 IP/账号在进程内限速；
- 已收发或以非草稿导入的邮件删除只改变该用户的 mailbox entry，不删除规范化管理员归档消息；私有草稿见下文例外。

### 队列和出站

外域消息按收件人保存到 SQLite 队列。worker 在发送前使用对应域的 2048 位 RSA 私钥进行 DKIM `rsa-sha256` 签名。

优先路径是 smarthost：支持隐式 TLS 或 STARTTLS，并只实现 TLS 内的 SASL `AUTH PLAIN`。提供商必须支持该机制；当前不支持 LOGIN 或 OAuth2。没有 relay 且 `BRCLIO_DIRECT_DELIVERY=false` 时，外域消息不会被悄悄直投，而是在队列中报告配置错误并重试。

直接 MX 模式会查询 MX/A 并尝试 SMTP；它是实验能力，当前没有 MTA-STS/DANE 策略强制、信誉管理和成熟退信流水线。

## 数据模型与删除语义

核心概念是“规范化消息”和“用户邮箱副本”分离：

```text
canonical message (raw MIME, parsed metadata, attachments)
       |
       +-- mailbox entry: Alice / INBOX / flags / UID
       +-- mailbox entry: Bob / INBOX / flags / UID
       +-- queue recipient state
       +-- administrator archive view (reason-gated + audit)
```

普通用户移动、标记或 EXPUNGE 操作只影响自己的 mailbox entry。规范化消息、原始 MIME、envelope 元数据和附件继续保留，管理员可通过原因门槛进入归档查看；列表接口在查看前不返回正文和隐藏收件人，查看和附件访问写审计事件。

草稿是明确例外：它属于用户尚未发送的私有工作状态，不进入管理员归档；删除或替换最后一份草稿 mailbox entry 时，草稿 raw MIME 与附件会物理删除。只有已接收、已提交发送或作为非草稿导入的邮件按上述留存语义处理。

这满足产品定义的“用户删除后自己不可见、管理员仍可见”，但不是密码学不可变、WORM 或合规保全。拥有主机/数据库权限的 operator 可以修改或删除数据。

用户 quota 按该用户当前可见副本的逻辑 MIME 大小计算；全局归档/SQLite 存储保护则对 raw MIME、解码附件 BLOB、正文/全文索引等做保守物理估算（包含仍存在的草稿），并受 `BRCLIO_MAX_ARCHIVE_BYTES`（默认 100 GiB）限制。达到上限会拒绝新的消息存储，不能把它当成自动清理策略。数据库卷可用空间低于 `BRCLIO_MIN_FREE_DISK_BYTES`（默认 1 GiB）时也会拒收；这些保护仍不等于文件系统配额或容量监控。

## SQLite

数据库使用：

- WAL journal mode；
- `synchronous=FULL`；
- foreign keys；
- immediate write transaction 与 busy timeout；
- 内置全文索引；
- 数据库文件权限 `0600`、数据目录 `0700`；
- 启动时校验 SQLite 版本至少 `3.51.3`。

WAL 依赖同一主机上的共享内存协调，因此不支持网络文件系统或多主机共享；见 [SQLite WAL 官方文档](https://www.sqlite.org/wal.html)。系统没有外部 blob store，原始 MIME 和附件都计入 SQLite/本地磁盘容量。

## TLS 与证书

自动模式由内置 ACME HTTP-01 获取证书；systemd 默认缓存到 `/var/lib/brclio-mail/acme`，Docker 缓存到 `/data/acme`。静态模式从 `/etc/brclio-mail/tls` 或容器只读挂载读取证书和私钥。Web HTTPS、Submission、SMTPS 和 IMAPS 共用 TLS 配置，最低 TLS 1.2。生产模式要求 HTTPS `BRCLIO_BASE_URL` 和可用证书。

邮件服务器之间的 SMTP 通常仍是逐跳安全，不是端到端加密。当前直接投递的 STARTTLS 不执行 MTA-STS/DANE 强制；敏感内容需要由发件人与收件人的更高层加密方案保护，而当前产品未内置这些方案。

## 信任与权限边界

- **普通用户**：只能访问自己的 mailbox entries、邮件和应用密码；
- **管理员**：管理所有域/用户/别名，并可读取全部管理员归档；
- **主机 operator**：可以读取/更改数据库、secret、备份和进程，是最高信任角色；
- **smarthost**：可看到出站 SMTP envelope 和逐跳明文内容，是外部受信任处理者；
- **DNS/CA/VPS 提供商**：控制路由、证书验证、PTR 和可达性。

管理员与主机 operator 不是零知识角色。部署者必须做权限分离、用户告知、备份加密和审计日志外送。

## 不支持的拓扑

- 两个或更多 Brclio Mail 副本指向同一数据库；
- NFS/SMB/CephFS/GlusterFS/对象存储 FUSE 上的 SQLite；
- 读写分离、自动 failover、跨地域 active-active；
- 仅靠同步运行中的 `.db` 文件实现灾备。

需要这些能力的部署不应使用当前 Preview 架构。
