# Brclio Mail

> **Preview / 预览版。** Brclio Mail 是面向个人与小团队的单节点私有邮件系统实验实现，尚未经过生产规模、长期兼容性或独立安全审计验证。请先阅读[已知限制与路线图](docs/limitations-roadmap.md)，不要把当前版本用于唯一副本、关键业务或合规归档。

Brclio Mail 把网页邮箱、管理员控制台、SMTP、Submission、IMAP、投递队列、不可由普通用户删除的管理员归档和审计日志放进一个 Go 服务，并使用本机 SQLite 持久化。它适合在一台有固定公网 IP、可配置 PTR 且开放 25 端口的 Linux 主机上试用。

## 当前能力

| 能力 | Preview 状态 |
| --- | --- |
| 网页邮箱与管理员控制台 | 已实现 |
| 域名、用户、别名、邮箱容量与应用专用密码 | 已实现 |
| SMTP 25 入站、Submission 465/587、IMAPS 993 | 已实现 |
| 本地域收发、队列重试、附件、草稿 | 已实现 |
| 出站 smarthost | 已实现且推荐 |
| 直接投递到收件方 MX | 实验性能力，默认关闭 |
| DKIM 出站签名与 DNS 记录提示 | 已实现 |
| 用户删除与管理员不可变归档分离 | 已实现；管理员列表元数据访问会审计，正文与附件需填写原因并审计 |
| SQLite 在线一致性备份与自检 CLI | 已实现 |
| SPF/DKIM/DMARC 入站验证、反垃圾、杀毒 | **尚未实现** |
| MTA-STS、DANE、S/MIME、端到端加密 | **尚未实现** |
| 高可用、多副本、NFS/共享文件系统 | **不支持** |

这里的“管理员不可变归档”不是法律意义上的 WORM、电子取证或合规归档。管理员可以读取所有往来邮件，包括普通用户已从自己邮箱删除的邮件；部署者必须提前向用户披露，并遵守当地隐私、劳动与通信法律。

## 快速部署

前置条件：Docker Engine、Docker Compose v2、一个域名、一台具有固定公网 IP 的主机，以及能够设置反向 DNS 的云服务商。先确认主机和上游网络允许 TCP 25；很多云平台默认封禁出站 25。

```bash
cp .env.example .env
mkdir -p secrets/tls
openssl rand -base64 36 > secrets/setup_token
: > secrets/relay_password
chmod 700 secrets secrets/tls
chmod 600 secrets/setup_token secrets/relay_password
```

编辑 `.env`，至少填写 `BRCLIO_HOSTNAME`、`BRCLIO_BASE_URL` 和 `BRCLIO_ACME_EMAIL`。推荐配置 smarthost，并保持 `BRCLIO_DIRECT_DELIVERY=false`。容器以 UID/GID `10001` 运行；在 Linux 主机上让它可以读取 secret：

```bash
sudo chown -R 10001:10001 secrets
docker compose config
docker compose up -d --build
docker compose logs -f brclio-mail
```

浏览 `https://mail.example.com`，使用 `secrets/setup_token` 完成首次管理员初始化。初始化后应轮换该文件中的令牌，再重建容器。域名创建后会保持 `pending`：先发布管理台给出的 `_brclio-mail.<domain>` TXT，再在后台点击检查，只有状态变为 `verified` 才允许该域通过公网 SMTP 收发或外发。完整记录见 [DNS 配置](docs/dns.md)。

Compose 的公网端口映射固定为：

| 公网 | 容器 | 用途 |
| ---: | ---: | --- |
| 80 | 8080 | ACME HTTP-01 与 HTTPS 跳转 |
| 443 | 8443 | Web/API HTTPS |
| 25 | 2525 | SMTP 服务器间入站 |
| 465 | 2465 | 隐式 TLS Submission |
| 587 | 2587 | STARTTLS Submission |
| 993 | 2993 | 隐式 TLS IMAP |

`docker compose` 使用本地 named volume 保存 `/data`。**不要**把数据库目录放到 NFS、SMB、对象存储 FUSE 或多台主机共享卷，也不要同时启动多个服务副本。SQLite WAL 只支持同一主机上的协作进程。

## TLS

默认示例使用内置 ACME HTTP-01：

```dotenv
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
BRCLIO_TLS_CERT=
BRCLIO_TLS_KEY=
```

若使用已有证书，将证书和私钥放进 `secrets/tls/`，使 UID `10001` 可读，然后改为：

```dotenv
BRCLIO_AUTO_TLS=false
BRCLIO_ACME_EMAIL=
BRCLIO_TLS_CERT=/run/tls/fullchain.pem
BRCLIO_TLS_KEY=/run/tls/privkey.pem
```

两种模式不能同时启用。完整步骤、续期要求与防火墙提示见[部署指南](docs/deployment.md)。

## 关键运行配置

| 变量 | 默认值 | 含义 |
| --- | ---: | --- |
| `BRCLIO_MAX_MESSAGE_BYTES` | `26214400`（25 MiB） | 单封原始 MIME 上限 |
| `BRCLIO_MAX_ARCHIVE_BYTES` | `107374182400`（100 GiB） | 管理员归档的保守物理占用估算上限 |
| `BRCLIO_MIN_FREE_DISK_BYTES` | `1073741824`（1 GiB） | 数据库所在卷低于该可用空间时拒绝新邮件 |
| `BRCLIO_DIRECT_DELIVERY` | `false` | 实验性直接 MX 投递开关；保持关闭并使用 smarthost |

用户邮箱 quota 是该用户当前可见副本的**逻辑 MIME 大小**，不是磁盘配额。归档 cap 会对 raw MIME、解码附件 BLOB、正文/全文索引等采用保守物理估算，也不等同于 `du` 显示的精确 SQLite 文件大小。1 GiB 低水位用于避免吃尽数据卷，但 WAL、备份、ACME 缓存及文件系统开销仍可能增长，必须独立监控容量并保留恢复空间。

## 运维入口

```bash
# 运行数据库、外键、配置和版本自检
docker compose exec -T brclio-mail brclio-mail doctor

# 创建经过完整性校验的 SQLite 在线备份
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
docker compose exec -T brclio-mail brclio-mail backup "/data/backups/${stamp}.sqlite"
mkdir -p backups
docker compose cp "brclio-mail:/data/backups/${stamp}.sqlite" "backups/${stamp}.sqlite"
```

备份仍包含所有邮件正文、附件、密码哈希、DKIM 私钥和管理员归档，必须加密并移出主机。恢复演练与校验命令见[运维、备份与恢复](docs/operations.md)。

## 文档

- [部署与 TLS](docs/deployment.md)
- [DNS、PTR 与发信信誉](docs/dns.md)
- [第三方客户端配置](docs/clients.md)
- [运维、备份与恢复](docs/operations.md)
- [架构](docs/architecture.md)
- [威胁模型](docs/threat-model.md)
- [限制与路线图](docs/limitations-roadmap.md)
- [贡献指南](CONTRIBUTING.md)
- [安全策略](SECURITY.md)

## 本地开发与核验

项目 CI 固定使用 Go `1.26.6`：

```bash
make fmt-check
make test
make test-race
make vet
make vuln
make build
docker build -t brclio-mail:preview .
```

开发模式可以在回环地址上不启用邮件 TLS；它不应暴露到公网。生产模式要求 HTTPS 和 TLS 证书。

## 许可

后端、协议实现与基础设施代码采用 `AGPL-3.0-or-later`。由 Esther Design System 衍生的 UI/视觉设计层采用 `CC BY-NC-SA 4.0`，要求署名、相同方式共享且仅限非商业用途。**商业部署或商业再分发必须替换该 UI/设计层**；AGPL 代码本身仍受 AGPL 网络交互源代码义务约束。由于 NC 限制，当前完整发行物不是一个全部符合 OSI 开源定义的单一许可作品；应准确表述为“AGPL 开源后端 + 非商业共享 UI”。项目条款见 [NOTICE](NOTICE) 和 [LICENSES/](LICENSES/)，生产二进制依赖的完整许可与通知见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES)。
