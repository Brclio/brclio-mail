# 部署与 TLS

## 0. 先判断是否适合

当前版本只支持一台主机、一个服务副本和本地 SQLite。它不支持 NFS/SMB、Kubernetes 多副本、共享卷或自动故障转移。邮件数据和管理员归档不能只有这一份。

主机至少需要：

- 64 位 Linux、Docker Engine 与 Docker Compose v2；
- 固定公网 IPv4，若发布 AAAA 则还必须有可双向连通的固定 IPv6；
- 云服务商可设置 PTR（反向 DNS）；
- 入站 TCP 25、80、443、465、587、993 可达；
- 若启用直接 MX 投递，出站 TCP 25 也必须可达。默认推荐 smarthost，因此可避免依赖出站 25，但接收互联网邮件仍需要入站 25；
- `mail.example.com` 的 A/AAAA 已指向这台主机，且 80/443 没有被其他服务占用（自动 ACME 模式）。

许多云厂商会默认封禁 25 端口。先向服务商确认，不能只看本机防火墙。SMTP 服务器间传输使用 25，客户端发信应使用 465 或 587；相关协议分别见 [RFC 5321](https://www.rfc-editor.org/rfc/rfc5321.html)、[RFC 6409](https://www.rfc-editor.org/rfc/rfc6409.html) 与 [RFC 8314](https://www.rfc-editor.org/rfc/rfc8314.html)。

## 1. 准备配置和 secret

```bash
git clone https://github.com/Brclio/brclio-mail.git
cd brclio-mail
cp .env.example .env
mkdir -p secrets/tls
openssl rand -base64 36 > secrets/setup_token
: > secrets/relay_password
chmod 700 secrets secrets/tls
chmod 600 secrets/setup_token secrets/relay_password
```

编辑 `.env`，替换所有 `example.com` 和 `replace-me`。如果暂时没有 smarthost，必须把 relay 地址、用户名和密码留空；外域邮件会留在队列并重试，因为 `BRCLIO_DIRECT_DELIVERY=false`。

容器固定以 UID/GID `10001` 运行。Compose 的 file secret 与静态证书必须可被该 UID 读取：

```bash
sudo chown -R 10001:10001 secrets
```

不要把 `.env`、`secrets/` 或数据库提交到 Git。首次初始化令牌至少应有 32 字节随机性；系统完成初始化后，旧令牌不再能重复初始化，但仍应轮换 secret 文件并重建容器。

## 2A. 自动 ACME（推荐入门）

配置：

```dotenv
BRCLIO_HOSTNAME=mail.example.com
BRCLIO_BASE_URL=https://mail.example.com
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
BRCLIO_TLS_CERT=
BRCLIO_TLS_KEY=
```

内置 ACME 客户端使用 HTTP-01 挑战。公网 TCP 80 必须直达容器的 8080，公网 TCP 443 直达 8443；不要让 CDN 代理或另一个 HTTP 服务截获 `/.well-known/acme-challenge/`。证书缓存保存在 `/data/acme` 的同一 named volume 中。

## 2B. 静态证书

将完整证书链和私钥放到 `secrets/tls/`：

```bash
sudo install -o 10001 -g 10001 -m 0644 /path/to/fullchain.pem secrets/tls/fullchain.pem
sudo install -o 10001 -g 10001 -m 0600 /path/to/privkey.pem secrets/tls/privkey.pem
```

配置：

```dotenv
BRCLIO_AUTO_TLS=false
BRCLIO_ACME_EMAIL=
BRCLIO_TLS_CERT=/run/tls/fullchain.pem
BRCLIO_TLS_KEY=/run/tls/privkey.pem
```

证书必须覆盖 `BRCLIO_HOSTNAME`。应用会把同一套证书用于 Web、465、587 STARTTLS 和 993，并要求 TLS 1.2 或更新版本。证书续期后执行：

```bash
docker compose up -d --force-recreate brclio-mail
```

自动 TLS 与静态证书互斥；证书和私钥必须同时配置。

## 3. 配置 smarthost

默认且推荐的出站模式是受信任的 authenticated smarthost：

```dotenv
BRCLIO_RELAY_ADDR=smtp.provider.example:465
BRCLIO_RELAY_USERNAME=account@example.com
BRCLIO_RELAY_IMPLICIT_TLS=true
BRCLIO_DIRECT_DELIVERY=false
```

把真实密码写入 `secrets/relay_password`，文件结尾换行会被自动去除。若提供商要求 587 + STARTTLS，改成 `:587` 与 `BRCLIO_RELAY_IMPLICIT_TLS=false`。当前客户端只实现 TLS 内的 SASL `AUTH PLAIN`，不支持 LOGIN 或 OAuth2；必须确认提供商允许 PLAIN over TLS。以提供商官方参数为准。

不要同时通过 `.env` 设置 `BRCLIO_RELAY_PASSWORD`；容器已经通过 `BRCLIO_RELAY_PASSWORD_FILE` 读取 secret。SPF 必须按 smarthost 提供商的官方说明授权其出口，详见 [DNS 配置](dns.md)。

`BRCLIO_DIRECT_DELIVERY=true` 会启用实验性的直接 MX 投递。当前实现没有 MTA-STS/DANE 强制、信誉预热、反馈回路或完整退信处理，不建议公开部署启用。

## 4. 启动

```bash
docker compose config
docker compose up -d --build
docker compose ps
docker compose logs -f brclio-mail
```

Compose 对外发布的端口为：

| 主机端口 | 容器端口 | 服务 |
| ---: | ---: | --- |
| 80 | 8080 | ACME HTTP-01 / HTTPS 跳转 |
| 443 | 8443 | Web/API HTTPS |
| 25 | 2525 | SMTP 入站，不提供客户端 AUTH |
| 465 | 2465 | SMTP Submission，隐式 TLS |
| 587 | 2587 | SMTP Submission，STARTTLS 后 AUTH |
| 993 | 2993 | IMAP，隐式 TLS |

Docker 发布端口可能绕过某些基于 UFW 的规则；应同时使用云防火墙/安全组并理解 Docker 的[端口发布](https://docs.docker.com/engine/network/port-publishing/)与[防火墙规则](https://docs.docker.com/engine/network/firewall-iptables/)行为。不要公开数据库文件或容器内部端口。

## 5. 首次初始化

1. 打开 `https://mail.example.com`。
2. 读取 `secrets/setup_token`，创建首个管理员和邮件域。
3. 在管理员控制台复制 `_brclio-mail.<domain>` 所有权 TXT 和 DKIM 公钥，发布 [DNS 记录](dns.md)。
4. DNS 生效后回到后台点击域名检查，确认状态由 `pending` 变为 `verified`。未验证域可以预先分配账号，但不能通过公网 SMTP 收信或通过 Submission 发信，也不能从 Web 向外域发信/进入外发队列。
5. 创建用户、容量限制与别名。首次初始化的域会自动把 `postmaster`、`abuse`、`security`、`hostmaster`、`dmarc` 和 `tlsrpt` 角色地址指向首位管理员；新增域需手动为这些地址创建受监控用户或别名。
6. 为每个第三方客户端创建独立应用专用密码，不要复用管理员主密码。

## 6. 部署验收

在外部网络运行，不能只在服务器本机测试：

```bash
curl -fsS https://mail.example.com/healthz
openssl s_client -connect mail.example.com:443 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:465 -servername mail.example.com </dev/null
openssl s_client -starttls smtp -connect mail.example.com:587 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:993 -servername mail.example.com </dev/null
nc -vz mail.example.com 25
docker compose exec -T brclio-mail brclio-mail doctor
```

`doctor` 应报告 `status: ok`、SQLite 完整性通过、配置的 TLS/投递模式和当前 SQLite 版本。当前程序拒绝低于 SQLite `3.51.3` 的运行时；Docker 构建使用项目锁定的 Go 依赖所内嵌的 SQLite。

先确认管理台域名状态为 `verified`，再从一个外部邮箱向本地域发信，并从 Brclio Mail 客户端回信。检查：入站、Sent、队列、DKIM 验证、SPF/DMARC 报告邮箱、普通用户删除后的管理员归档和审计事件。DNS 层验收见 [dns.md](dns.md)。

## 7. 数据卷约束

Compose volume `brclio-mail-data` 必须是同一台主机的本地块存储。SQLite WAL 要求所有读写者位于同一主机，官方文档明确指出 WAL 不适用于网络文件系统；见 [SQLite WAL](https://www.sqlite.org/wal.html)。

- 副本数必须保持 `1`；
- 不要把 `/data` 绑定到 NFS、SMB、CephFS、GlusterFS 或对象存储 FUSE；
- 不要在两台容器主机间共享该目录；
- 迁移前先停止旧实例并完成备份/恢复验收；
- 不要直接复制一个正在运行的 `.db`、`-wal`、`-shm` 文件组作为备份，使用内置 `backup` CLI。
