# 部署与 TLS

## 0. 先判断是否适合

Brclio Mail Preview 面向个人、家庭和完全受信任的小公司/小团队，在一台自己可管理的 Linux 主机上运行。当前版本只支持单主机、单服务副本和本地 SQLite；它不是托管邮件平台，也不支持 NFS/SMB、Kubernetes 多副本、共享数据库或自动故障转移。邮件数据和管理员归档不能只有这一份。

许可边界同样需要在部署前确认：后端采用 AGPL-3.0-or-later，但 Esther 衍生 UI 使用 CC BY-NC-SA 4.0、禁止商用。营利性公司若要把系统用于业务，必须先替换该 UI/设计层并履行 AGPL 义务；当前完整界面只适合非商业试用，详见仓库 `NOTICE` 与 `LICENSES/`。

**首选部署方式是原生 Linux + systemd。** 本文主线适用于带 systemd 的 Ubuntu/Debian 与 RHEL/Rocky/AlmaLinux 等发行版；Docker Compose 保留为[可选方式](#8-可选docker-compose)，但不会改变单机边界或 Preview 成熟度。

需要线性图文步骤时，直接选择：[宝塔面板部署](tutorial-baota.md)、[命令行与一键部署](tutorial-command-line.md)或[Docker Compose 部署](tutorial-docker.md)。本文继续作为 TLS、路径、端口和生命周期的详细参考。

主机至少需要：

- 64 位 Linux、systemd、root/sudo 权限和本地块存储；
- 固定公网 IPv4，若发布 AAAA 则还必须有双向可达的固定 IPv6；
- 云服务商允许设置 PTR（反向 DNS）；
- 入站 TCP `25/80/443/465/587/993` 可达；
- 若启用实验性直接 MX 投递，出站 TCP 25 也必须可达；推荐 smarthost，因此通常不依赖出站 25，但接收互联网邮件仍需要入站 25；
- `mail.example.com` 的 A/AAAA 已指向这台主机，自动 ACME 模式下 80/443 没有被其他服务占用。

许多云厂商默认封禁 25 端口。先向服务商确认，不能只看主机防火墙。SMTP 服务器间传输使用 25，客户端发信应使用 465 或 587；相关协议分别见 [RFC 5321](https://www.rfc-editor.org/rfc/rfc5321.html)、[RFC 6409](https://www.rfc-editor.org/rfc/rfc6409.html) 与 [RFC 8314](https://www.rfc-editor.org/rfc/rfc8314.html)。

## 1. 安装主机依赖

安装脚本要求 systemd `247` 或更新版本。Ubuntu 20.04、RHEL 8 等自带更旧 systemd 的系统不在这条安装路径的支持范围；不要为了本 Preview 非官方替换发行版的核心 systemd。先核对：

```bash
systemctl --version | head -n 1
```

Ubuntu/Debian：

```bash
sudo apt-get update
sudo apt-get install -y git curl ca-certificates openssl tar coreutils util-linux passwd sqlite3
```

RHEL/Rocky/AlmaLinux：

```bash
sudo dnf install -y git curl ca-certificates openssl tar coreutils util-linux shadow-utils sqlite
```

默认安装程序下载指定 GitHub Release 的 `amd64`/`arm64` 包，并用该 Release 的 `checksums.txt` 核对 SHA-256，因此不需要 Go。不要从未知镜像站下载二进制，也不要把远程脚本直接 pipe 给 root。

只有需要自行审阅并从本地源码构建时，才安装项目固定的 Go `1.26.6`。发行版仓库中的 Go 版本可能较旧；按 [Go 官方安装说明](https://go.dev/doc/install)安装后核对并构建：

```bash
go version
test "$(go env GOVERSION)" = "go1.26.6"
make build
```

## 2. 首选：安装 systemd 服务

Release 路线应检出准备部署的明确 tag，先查看变更和安装脚本，再执行安装；若审阅的是某个未发布 commit，必须自行构建并改用 `--binary`：

```bash
target_version="v0.2.1-preview"
git clone --branch "$target_version" --depth 1 https://github.com/Brclio/brclio-mail.git
cd brclio-mail
git status --short
sudo ./scripts/install-systemd.sh \
  --version "$target_version" \
  --hostname mail.example.com \
  --acme-email postmaster@example.com \
  --no-start
```

`target_version` 只是当前 Preview 示例；部署时应把 checkout 与 `--version` 同时改成已审阅的同一 Release tag。`--no-start` 是唯一可靠的禁止自动启动门禁，让管理员先检查配置、防火墙和 DNS。若同时提供 `--hostname` 与 `--acme-email` 且不加该选项，首次安装会默认 `enable --now`；即使不传这两个参数，只要管理员已经预置无示例占位符的就绪 env，安装器仍可能启动。用本地构建时改为 `--binary "$PWD/bin/brclio-mail"`；若从 Release tar 解压后直接运行包内脚本且不传目标参数，脚本会使用同包二进制，不会下载硬编码的旧版本。首次安装不会覆盖预先存在的配置或 secret，缺少时会生成随机 `setup_token` 和空的 `relay_password`；检测到现有二进制、unit 或数据库时，直接 installer 会拒绝覆盖，必须使用第 9 节的事务式升级脚本。

默认布局：

| 内容 | 路径 |
| --- | --- |
| 服务用户 | `brclio-mail` |
| 二进制 | `/usr/local/bin/brclio-mail` |
| systemd units | `/etc/systemd/system/brclio-mail.service`、`brclio-mail-doctor.service`、`brclio-mail-backup@.service` |
| root backup helper | `/usr/local/libexec/brclio-mail-backup-helper` |
| 环境配置 | `/etc/brclio-mail/brclio-mail.env` |
| setup/relay secret | `/etc/brclio-mail/secrets/` |
| 静态证书 | `/etc/brclio-mail/tls/` |
| SQLite、ACME cache | `/var/lib/brclio-mail/` |
| root 管理的本机备份区 | `/var/backups/brclio-mail/` |

仓库中的模板与 unit 分别位于 [packaging/brclio-mail.env.example](../packaging/brclio-mail.env.example) 和 [packaging/systemd/brclio-mail.service](../packaging/systemd/brclio-mail.service)。服务不以 root 运行；unit 只授予绑定低端口所需的 `CAP_NET_BIND_SERVICE`。安装脚本是部署便利工具，不代表发行版认证、自动补丁、监控、反垃圾或高可用已经具备。

## 3. 检查配置和 secret

在首次启动前编辑：

```bash
sudoedit /etc/brclio-mail/brclio-mail.env
```

systemd 模板默认直接监听公网标准端口：Web/ACME `80/443`、SMTP `25`、Submission `465/587`、IMAPS `993`；明文 IMAP 保持禁用。至少核对：

```dotenv
BRCLIO_HOSTNAME=mail.example.com
BRCLIO_BASE_URL=https://mail.example.com
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
BRCLIO_DIRECT_DELIVERY=false
BRCLIO_DEV_MODE=false
BRCLIO_DISABLE_MAIL_SERVERS=false
```

不要把 `BRCLIO_DEV_MODE` 用于公网部署。受安装/升级脚本管理的 systemd 部署必须保持这里的 `/var/lib/brclio-mail` 数据目录和默认数据库路径；升级脚本会拒绝非标准路径。需要其他单机本地文件系统布局时，应先设计并演练独立迁移/升级流程，仍不能指向网络共享。

确认 secret 源文件存在且只有 root 可读：

```bash
sudo test -s /etc/brclio-mail/secrets/setup_token
sudo stat -c '%U:%G %a %n' /etc/brclio-mail/secrets/setup_token
sudo stat -c '%U:%G %a %n' /etc/brclio-mail/secrets/relay_password
```

密码和 token 只放在这些 root 所有的文件中，不要写进环境配置、shell history、日志或 Git。安装包提供的 unit 使用 systemd `LoadCredential` 为非 root 服务进程创建隔离的只读副本；不要在 `brclio-mail.env` 中自行定义 `BRCLIO_SETUP_TOKEN_FILE` 或 `BRCLIO_RELAY_PASSWORD_FILE` 覆盖它。若使用 smarthost，通过 `sudoedit /etc/brclio-mail/secrets/relay_password` 填写真实密码。

## 4. TLS

### 4A. 自动 ACME（推荐入门）

使用上面的 `BRCLIO_AUTO_TLS=true` 配置。内置 ACME 客户端采用 HTTP-01；公网 80 必须直接到本机 80，443 直接到本机 443，不要让 CDN 或另一 HTTP 服务截获 `/.well-known/acme-challenge/`。证书缓存位于 `/var/lib/brclio-mail/acme`，和 SQLite 一样需要本地磁盘与备份容量规划。

### 4B. 静态证书

安装完整证书链与私钥，并启用随软件安装的 systemd credential drop-in。源私钥保持 root-only；不要把服务账号加入可长期读取私钥的组：

```bash
sudo install -d -o root -g root -m 0700 /etc/brclio-mail/tls
sudo install -o root -g root -m 0600 /path/to/fullchain.pem /etc/brclio-mail/tls/fullchain.pem
sudo install -o root -g root -m 0600 /path/to/privkey.pem /etc/brclio-mail/tls/privkey.pem
for unit in brclio-mail.service brclio-mail-doctor.service; do
  sudo install -d -o root -g root -m 0755 "/etc/systemd/system/${unit}.d"
  sudo install -o root -g root -m 0644 \
    /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf \
    "/etc/systemd/system/${unit}.d/20-static-tls.conf"
done
```

然后配置：

```dotenv
BRCLIO_AUTO_TLS=false
BRCLIO_ACME_EMAIL=
```

不要在环境文件中设置 `BRCLIO_TLS_CERT`/`BRCLIO_TLS_KEY`；drop-in 会把 credential 临时路径注入主服务，并让 doctor 正确报告静态 TLS 模式。执行 `sudo systemctl daemon-reload && sudo systemctl try-restart brclio-mail` 后，从外部检查所有 TLS 端口；尚未启动的首次安装仍按第 6 节启动。证书必须覆盖 `BRCLIO_HOSTNAME`；Web、465、587 STARTTLS 和 993 共用这套证书，最低 TLS 1.2。续期工具原子替换源文件后也必须重启并复查。自动 TLS 与静态证书互斥，证书和私钥必须同时配置；切回 ACME 前，要从两个 unit 的 `.d/` 目录删除 `20-static-tls.conf`，再执行 `daemon-reload` 和重启。

## 5. 配置 smarthost

推荐让外域邮件经过受信任的 authenticated smarthost：

```dotenv
BRCLIO_RELAY_ADDR=smtp.provider.example:465
BRCLIO_RELAY_USERNAME=account@example.com
BRCLIO_RELAY_IMPLICIT_TLS=true
BRCLIO_DIRECT_DELIVERY=false
```

真实密码只写入 `/etc/brclio-mail/secrets/relay_password`。若提供商要求 587 + STARTTLS，使用 `:587` 与 `BRCLIO_RELAY_IMPLICIT_TLS=false`。出站 relay 只实现 TLS 内的 SASL `AUTH PLAIN`，不支持 LOGIN 或 OAuth2；必须确认提供商允许 PLAIN over TLS。SPF 必须按提供商官方说明授权真实出口，详见 [DNS 配置](dns.md)。

`BRCLIO_DIRECT_DELIVERY=true` 会启用实验性直接 MX 投递。当前没有 MTA-STS/DANE 强制、信誉预热、反馈回路或完整退信处理，不建议公开部署启用。

## 6. 防火墙、启动与首次初始化

同时配置云安全组和主机防火墙。UFW 示例：

```bash
for port in 25 80 443 465 587 993; do sudo ufw allow "${port}/tcp"; done
sudo ufw status verbose
```

firewalld 示例：

```bash
for port in 25 80 443 465 587 993; do sudo firewall-cmd --permanent --add-port="${port}/tcp"; done
sudo firewall-cmd --reload
sudo firewall-cmd --list-ports
```

不要关闭 SELinux；在 enforcing 模式下检查安装脚本创建的标准路径标签和任何 AVC 拒绝。完成配置后启动：

```bash
sudo systemctl daemon-reload
if ! sudo systemctl start brclio-mail-doctor.service; then
  sudo journalctl -u brclio-mail-doctor.service -n 100 --no-pager
  echo 'doctor failed; public listeners remain stopped' >&2
  exit 1
fi
doctor_invocation_id="$(sudo systemctl show brclio-mail-doctor.service \
  --property=InvocationID --value)"
[[ -n "$doctor_invocation_id" ]] || {
  echo 'cannot resolve the current doctor invocation' >&2
  exit 1
}
doctor_report="$(sudo journalctl \
  "_SYSTEMD_INVOCATION_ID=${doctor_invocation_id}" --no-pager -o cat)"
printf '%s\n' "$doctor_report"
if grep -F '"deliveryMode":"disabled"' <<<"$doctor_report"; then
  echo 'configure smarthost or explicitly reviewed direct MX before starting' >&2
  exit 1
fi
sudo systemctl enable --now brclio-mail
sudo systemctl status brclio-mail --no-pager
sudo journalctl -u brclio-mail -n 100 --no-pager
/usr/local/bin/brclio-mail version
```

打开 `https://mail.example.com`，读取 `/etc/brclio-mail/secrets/setup_token` 完成首次管理员初始化。初始化后应轮换该 token 并重启服务。域名最初保持 `pending`：发布管理台给出的 `_brclio-mail.<domain>` TXT，回到后台点击检查；只有变为 `verified` 后，才允许该域通过公网 SMTP 收信或外发。

首次初始化的域会自动把 `postmaster`、`abuse`、`security`、`hostmaster`、`dmarc` 和 `tlsrpt` 角色地址指向首位管理员；新增域需手动创建受监控用户或别名。为每个第三方客户端创建独立应用专用密码，不要复用管理员主密码。

## 7. 部署验收与磁盘边界

从外部网络运行，不能只在服务器本机测试：

```bash
curl -fsS https://mail.example.com/healthz
openssl s_client -connect mail.example.com:443 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:465 -servername mail.example.com </dev/null
openssl s_client -starttls smtp -connect mail.example.com:587 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:993 -servername mail.example.com </dev/null
nc -vz mail.example.com 25
sudo systemctl is-active brclio-mail
```

按[运维文档](operations.md)运行 `doctor`，并先确认管理台域名为 `verified`，再做真实外部收发。检查入站、Sent、队列、DKIM/SPF/DMARC 结果、角色邮箱、普通用户删除后的管理员归档和审计事件。

`/var/lib/brclio-mail` 必须位于这台主机的本地块存储。SQLite WAL 不适用于网络文件系统，见 [SQLite WAL](https://www.sqlite.org/wal.html)：

- 服务副本数必须保持 `1`；
- 不要使用 NFS、SMB、CephFS、GlusterFS 或对象存储 FUSE；
- 不要让 systemd 服务和 Docker 容器同时指向同一个数据库；
- 迁移前停止旧实例，并完成备份、恢复和回滚验收；
- 不要复制运行中的 `.db`、`-wal`、`-shm` 文件组作为备份，使用内置 `backup` CLI。

## 8. 可选：Docker Compose

需要容器隔离或已有 Docker 运维体系时，可以继续使用 Compose。它是可选部署面，不是默认建议，也不能提供高可用。

完整的首次安装、TLS、volume、备份、恢复和升级流程见 [Docker Compose 独立教程](tutorial-docker.md)。这里不再提供可独立粘贴的“最小启动块”：跳过该教程第 6 节的既有 volume 拒绝、DockerRootDir/实际文件系统门禁和预启动 doctor，可能把新配置直接连接到旧邮件库或网络/FUSE 卷。必须从独立教程第 1 节顺序执行到第 7 节，不能只摘取最后的 `docker compose up`。

Compose 使用 `/data` named volume，并把主机 `80/443/25/465/587/993` 映射到容器 `8080/8443/2525/2465/2587/2993`。静态证书路径应使用 `/run/tls/fullchain.pem` 与 `/run/tls/privkey.pem`；复制证书后还要单独核对 `10001:10001`、`0600`，不能依赖之前的递归 `chown`。Docker 发布端口可能绕过部分 UFW 规则；应理解 Docker 的[端口发布](https://docs.docker.com/engine/network/port-publishing/)和[防火墙规则](https://docs.docker.com/engine/network/firewall-iptables/)行为，并同时配置云防火墙。

systemd 与 Docker 的配置文件、secret 路径和数据路径不同，不能直接混用。完整 Docker 运维命令见[运维文档](operations.md#docker-compose-运维对照)。

## 9. 升级与卸载入口

升级前先按[运维文档](operations.md)创建并移出一份备份，再检出明确的目标 tag/commit。Release 安装示例：

```bash
target_version="v0.2.1-preview"
git fetch --tags
git checkout "$target_version"
sudo ./scripts/upgrade-systemd.sh --version "$target_version"
/usr/local/bin/brclio-mail version
```

从当前 checkout 本地构建时，先核对 Go 版本并运行 `make build`，再传 `--binary "$PWD/bin/brclio-mail"`。Release 升级会先在旧服务仍可用时下载、核对 checksum 与二进制版本，之后才停止 active 服务；确认已停止后，用当前旧二进制创建一致性时间戳 SQLite 快照和旧 package 快照。安装完成后先在服务仍停止时运行 `doctor`，只有成功才在原服务此前 active 时重新启动并核验状态。安装、预启动 doctor 或可捕获的 HUP/INT/TERM 中断会在监听器仍关闭时成对恢复旧数据库和 package；监听器重新开放后的状态/HTTPS 检查失败不会自动恢复旧快照，以免静默丢弃升级后新邮件。断电与 SIGKILL 仍只能依靠打印的成对快照人工恢复。升级因此有短暂停机。自动本机备份不能替代异地加密副本；任何手工回滚也必须成对恢复匹配的旧 package 和 pre-upgrade SQLite 快照。

卸载服务入口：

```bash
sudo ./scripts/uninstall-systemd.sh
```

默认卸载会先停服，用仍安装的旧二进制发布 root-only 的 `pre-uninstall-*.sqlite`，并保存同时间戳旧 package；任一步失败都不会继续移除，并会尝试恢复先前可用性。成功后才移除安装的 unit 与二进制，同时保留 `/etc/brclio-mail`、`/var/lib/brclio-mail`、`/var/backups/brclio-mail`、手工创建的 static TLS drop-in 和服务账号。脚本会打印用该快照在保留数据上重装、运行 doctor、再开放监听器的恢复命令。先阅读脚本与将要删除的精确路径；数据清理必须单独、显式执行，并先验证可恢复备份。
