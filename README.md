# Brclio Mail

> **Preview / 预览版。** Brclio Mail 是面向个人、家庭与完全受信任的小公司/小团队的单节点私有邮件系统实验实现，尚未经过生产规模、长期兼容性或独立安全审计验证。请先阅读[已知限制与路线图](docs/limitations-roadmap.md)，不要把当前版本用于唯一副本、关键业务或合规归档。

Brclio Mail 把网页邮箱、管理员控制台、SMTP、Submission、IMAP、投递队列、不可由普通用户删除的管理员归档和审计日志放进一个 Go 服务，并使用本机 SQLite 持久化。它适合在一台有固定公网 IP、可配置 PTR 且开放 25 端口的 Linux 主机上，由个人或小公司管理员进行低风险试用。

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

首选方式是直接安装为 Linux systemd 服务；示例适用于 systemd `247` 或更新版本的 Ubuntu/Debian 与 RHEL/Rocky/AlmaLinux。前置条件包括常用系统工具、一个域名、一台具有固定公网 IP 的主机，以及能够设置反向 DNS 的云服务商。先确认上游网络允许入站 TCP 25；很多云平台还会封禁出站 25。安装程序默认下载指定 GitHub Release 并核对 SHA-256；只有选择本地源码构建时才需要 Go `1.26.6`。

```bash
target_version="v0.2.0-preview"
git clone --branch "$target_version" --depth 1 https://github.com/Brclio/brclio-mail.git
cd brclio-mail
sudo ./scripts/install-systemd.sh \
  --version "$target_version" \
  --hostname mail.example.com \
  --acme-email postmaster@example.com \
  --no-start
sudoedit /etc/brclio-mail/brclio-mail.env
sudo systemctl enable --now brclio-mail
sudo systemctl status brclio-mail --no-pager
/usr/local/bin/brclio-mail version
```

安装程序创建受限的 `brclio-mail` 服务账号，配置位于 `/etc/brclio-mail`，SQLite 与 ACME cache 位于 `/var/lib/brclio-mail`，并在缺失时生成 `/etc/brclio-mail/secrets/setup_token`。首次安装不会覆盖预先准备的配置或 secret；检测到现有安装时会拒绝直接重装，必须走受备份保护的 `upgrade-systemd.sh`。推荐配置 smarthost 并保持 `BRCLIO_DIRECT_DELIVERY=false`；完整的系统包、防火墙、TLS、secret 权限及 SELinux 提示见[部署指南](docs/deployment.md)。

浏览 `https://mail.example.com`，使用 setup token 完成首次管理员初始化。初始化后轮换 token 并重启服务。新域名保持 `pending`：先发布管理台给出的 `_brclio-mail.<domain>` TXT，再在后台点击检查，只有状态变为 `verified` 才允许该域通过公网 SMTP 收发或外发。完整记录见 [DNS 配置](docs/dns.md)。

systemd 服务直接监听标准端口：

| 端口 | 用途 |
| ---: | --- |
| 80 | ACME HTTP-01 与 HTTPS 跳转 |
| 443 | Web/API HTTPS |
| 25 | SMTP 服务器间入站 |
| 465 | 隐式 TLS Submission |
| 587 | STARTTLS Submission |
| 993 | 隐式 TLS IMAP |

Docker Compose 仍是[可选部署方式](docs/deployment.md#8-可选docker-compose)，不再是首选。无论采用 systemd 还是 Docker，数据库必须位于同一主机的本地块存储；**不要**使用 NFS、SMB、对象存储 FUSE、多主机共享卷或多个服务副本。

## TLS

默认示例使用内置 ACME HTTP-01：

```dotenv
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
```

若使用已有证书，将证书和私钥以 `root:root`、`0600` 放进 `/etc/brclio-mail/tls/`，把安装好的静态 TLS credential drop-in 同时启用到主服务和 doctor unit，然后改为：

```dotenv
BRCLIO_AUTO_TLS=false
BRCLIO_ACME_EMAIL=
```

不要在环境文件中直接设置证书、私钥或 secret 的 `_FILE` 路径；systemd unit 通过 `LoadCredential` 提供隔离副本。两种 TLS 模式不能同时启用。完整 drop-in 命令、续期要求与防火墙提示见[部署指南](docs/deployment.md)。

## 关键运行配置

| 变量 | 默认值 | 含义 |
| --- | ---: | --- |
| `BRCLIO_MAX_MESSAGE_BYTES` | `26214400`（25 MiB） | 单封原始 MIME 上限 |
| `BRCLIO_MAX_ARCHIVE_BYTES` | `107374182400`（100 GiB） | 管理员归档的保守物理占用估算上限 |
| `BRCLIO_MIN_FREE_DISK_BYTES` | `1073741824`（1 GiB） | 数据库所在卷低于该可用空间时拒绝新邮件 |
| `BRCLIO_BACKUP_TIMEOUT` | `2h` | 单次 SQLite backup CLI 的最长执行时间 |
| `BRCLIO_DIRECT_DELIVERY` | `false` | 实验性直接 MX 投递开关；保持关闭并使用 smarthost |

用户邮箱 quota 是该用户当前可见副本的**逻辑 MIME 大小**，不是磁盘配额。归档 cap 会对 raw MIME、解码附件 BLOB、正文/全文索引等采用保守物理估算，也不等同于 `du` 显示的精确 SQLite 文件大小。1 GiB 低水位用于避免吃尽数据卷，但 WAL、备份、ACME 缓存及文件系统开销仍可能增长，必须独立监控容量并保留恢复空间。

## 运维入口

```bash
sudo systemctl is-active brclio-mail
sudo journalctl -u brclio-mail --since "30 minutes ago"
curl -fsS https://mail.example.com/healthz
```

`doctor`、一致性在线备份、恢复、升级/回滚和 Docker 对照命令见[运维、备份与恢复](docs/operations.md)。备份仍包含所有邮件正文、附件、密码哈希、DKIM 私钥和管理员归档，必须加密并移出主机。

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
