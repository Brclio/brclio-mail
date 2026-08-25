# 宝塔面板部署 Brclio Mail：个人与小公司裸机安装教程

> 只想先完成首次上线？请使用[宝塔快速部署版](tutorial-baota-quick.md)。本文保留高级证书、排错、备份、升级和卸载细节。

本文演示如何在已经安装宝塔面板的 Linux 服务器上部署 Brclio Mail。最终结构不是“在宝塔里创建一个普通网站”，而是：

- 宝塔面板负责终端、防火墙、计划任务与日常观察；
- Brclio Mail 由 Linux `systemd` 直接托管；
- Web、SMTP Submission 与 IMAPS 使用公网标准端口；
- SQLite、ACME 缓存与本机备份保存在服务器本地磁盘。

![Bornforthis 把宝塔操作台接到 systemd 邮件机](../assets/baota-deployment-illustrations/01-panel-controls-systemd.png)

> 当前版本是 `v0.2.1-preview`，适合个人、家庭和完全受信任的小公司在单台服务器上试用。它不是高可用或合规归档系统，也尚未内置入站反垃圾、杀毒及 SPF/DKIM/DMARC 验证。不要把它作为关键邮件的唯一副本。

> **公司使用前先确认许可。** 后端代码按 AGPL 提供；当前完整界面包含 Esther Design System 衍生内容，仓库将该部分标记为 CC BY-NC-SA 4.0 非商业许可。商业经营场景必须替换相关 UI/设计或取得单独授权，并履行 AGPL 的网络源代码义务。

## 1. 先理解宝塔在这套部署中的角色

Brclio Mail 自己提供 HTTPS、SMTP、Submission 和 IMAP 服务，因此不要在宝塔“网站”菜单里把它当 PHP、Node.js 或静态网站部署，也不要默认再套一层 Nginx 反向代理。

最省事、最容易验收的做法是使用一台专用服务器：

```text
宝塔管理端口 ──> 宝塔面板

80/443        ──> Brclio Mail Web 与 ACME
25            ──> SMTP 服务器间收信
465/587       ──> 邮件客户端发信
993           ──> 邮件客户端收信

本地磁盘      ──> SQLite + ACME cache + 本机备份
```

如果是新安装宝塔，首次环境向导应跳过 LNMP/LAMP；Brclio Mail 不需要 PHP、MySQL、Nginx 或 Apache。如果这台服务器已经承载网站，Web 服务通常会占用 `80/443`，初次部署不要直接改端口、停生产网站或做复杂代理，优先换一台独立 VPS。

不要在宝塔“网站”中创建 `mail.example.com`，也不要设置普通网站反向代理。宝塔“网站”里的常规反向代理只处理 HTTP，不能覆盖 SMTP/Submission/IMAPS；当前 Web 限速与审计还直接读取连接 `RemoteAddr`，套代理会把客户端地址折叠成代理地址。

### 1.1 先保护宝塔面板自身

- 面板使用随机高位管理端口，只允许可信管理 IP；
- 面板端口和 SSH 不要向所有来源开放；
- 确认没有通过 `bt 32` 允许面板占用 `80/443`；
- `bt 1` 至 `bt 4` 只管理宝塔面板，不会管理 Brclio Mail；
- `bt 14` 会显示面板入口等敏感信息，只在自己的终端查看，不要复制到截图或工单；
- 开启面板 HTTPS、强认证和登录告警，并限制能使用 Web 终端的账号。

## 2. 服务器与域名前置条件

开始前确认：

- 64 位 Linux，CPU 为 `amd64/x86_64` 或 `arm64/aarch64`；
- `systemd` 版本不低于 `247`；
- 固定公网 IP，并能在云厂商设置 PTR 反向解析；
- `/var/lib/brclio-mail` 位于本机块存储，不是 NFS、SMB、对象存储 FUSE 或多机共享盘；
- 云厂商允许入站 TCP `25/80/443/465/587/993`；
- 若云厂商封禁出站 25，已经准备支持 TLS 内 `AUTH PLAIN` 的 smarthost；
- 已决定邮件主机名，例如 `mail.example.com`，且该主机名没有开启 CDN/代理。

在宝塔左侧进入“终端”，执行只读预检：

```bash
uname -m
systemctl --version | head -1
df -h / /var/lib
findmnt --target /var/lib
sudo ss -ltnp | grep -E ':(25|80|443|465|587|993)\b' || true
```

最后一条没有输出，表示目标端口暂时没有 TCP 监听；有输出时必须先确认对应进程。不要为了“让安装通过”直接杀死未知进程。

## 3. 先设置 A 记录与 PTR

内置 ACME 申请证书前，至少先完成：

```text
mail.example.com.  A    203.0.113.10
203.0.113.10       PTR  mail.example.com.
```

PTR 在 VPS 或云厂商控制台设置，不是在普通 DNS 解析区添加。只有确认 IPv6 的 `25/80/443/465/587/993` 双向可达时才发布 AAAA；错误的 AAAA 会让部分远端优先连接一个不可达地址。

## 4. 在宝塔和云安全组放行端口

进入宝塔左侧“安全” → “系统防火墙” → “添加端口规则”，建立允许规则：

| 协议 | 端口 | 来源 | 用途 |
| --- | --- | --- | --- |
| TCP | `25` | 所有 IP | SMTP 入站 |
| TCP | `80` | 所有 IP | ACME HTTP-01 与跳转 |
| TCP | `443` | 所有 IP | Web/API HTTPS |
| TCP | `465` | 所有 IP | 隐式 TLS Submission |
| TCP | `587` | 所有 IP | STARTTLS Submission |
| TCP | `993` | 所有 IP | IMAPS |

这些端口是公开邮件服务入口；宝塔管理端口和 SSH 端口应尽量只允许你的固定管理 IP。不要放行明文 IMAP `143`，Brclio Mail 的默认裸机配置没有启用它。

![Bornforthis 依次打开六道邮件端口闸门](../assets/baota-deployment-illustrations/02-open-mail-port-gates.png)

宝塔规则只处理服务器系统防火墙。阿里云、腾讯云、华为云等云安全组还要添加同样的 TCP 入站规则，否则宝塔可能显示“外网不通”。

同时审计并关闭宝塔安装环境遗留但本系统不需要的公网规则，例如 FTP、数据库 `3306`、phpMyAdmin 和 Docker 内部端口；云安全组中的对应规则也要同步删除。不要误删仍由其他已确认业务使用的入口。

## 5. 在宝塔终端安装依赖

宝塔终端通常已经是 root。若 `id -u` 不是 `0`，先执行 `sudo -i` 进入 root 管理 shell；后续 `cd /root`、文件权限与生命周期命令都以这个前提编写。示例仍保留 `sudo`，root 执行时不会改变结果。

Ubuntu/Debian：

```bash
sudo apt-get update
sudo apt-get install -y \
  bash git curl ca-certificates openssl tar coreutils util-linux \
  passwd sqlite3 grep sed findutils gawk iproute2 netcat-openbsd
```

RHEL/Rocky/AlmaLinux：

```bash
sudo dnf install -y \
  bash git curl ca-certificates openssl tar coreutils util-linux \
  shadow-utils sqlite grep sed findutils gawk iproute nmap-ncat
```

## 6. 下载明确版本并安装，但先不启动

不要对邮件服务器执行来源不明的 `curl | bash`。检出明确的 Release tag，查看状态后运行仓库内安装器：

```bash
bash <<'BRCLIO_BAOTA_INSTALL'
set -Eeuo pipefail
target_version="v0.2.1-preview"
mail_hostname="mail.example.com"
acme_email="postmaster@example.com"
[[ "$acme_email" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || {
  echo 'ACME email must be a conventional ASCII address' >&2
  exit 1
}
repo_dir="/root/brclio-mail"
[[ ! -e "$repo_dir" && ! -L "$repo_dir" ]] || {
  echo '/root/brclio-mail already exists; inspect it instead of reusing it' >&2
  exit 1
}
git clone --branch "$target_version" --depth 1 \
  https://github.com/Brclio/brclio-mail.git "$repo_dir"
cd "$repo_dir"
test -z "$(git status --porcelain)"
test "$(git describe --tags --exact-match)" = "$target_version"

sudo ./scripts/install-systemd.sh \
  --version "$target_version" \
  --hostname "$mail_hostname" \
  --acme-email "$acme_email" \
  --no-start
BRCLIO_BAOTA_INSTALL
```

把 `mail.example.com` 和 `postmaster@example.com` 全部替换为你的真实值。为减少面板粘贴输入错误，本文安装块只接受常规 ASCII ACME 邮箱；安装器本身已对 env 模板替换字符做转义并有包验证测试。安装器会自动识别 amd64/arm64，从 GitHub Release 下载对应包，核对 `checksums.txt` 中的 SHA-256，并验证二进制产品名和版本，然后创建受限服务账号及 systemd unit。

默认路径：

| 内容 | 路径 |
| --- | --- |
| 主配置 | `/etc/brclio-mail/brclio-mail.env` |
| setup/relay secret | `/etc/brclio-mail/secrets/` |
| 静态 TLS 文件 | `/etc/brclio-mail/tls/` |
| SQLite 与 ACME cache | `/var/lib/brclio-mail/` |
| root 管理的备份 | `/var/backups/brclio-mail/` |
| 服务日志 | `journalctl -u brclio-mail` |

安装后再次确认真实数据目录所在文件系统：

```bash
findmnt --target /var/lib/brclio-mail
```

它必须是当前服务器的本机存储，不能是 NFS、SMB 或 FUSE 共享盘。

## 7. 使用宝塔终端安全编辑配置

不要用宝塔文件管理上传来源不明的软件，或修改 live 配置、secret、证书私钥和 SQLite/WAL。本文只使用已经核对 tag、Release SHA-256 与二进制版本的安装器路线；配置统一在终端使用：

```bash
sudoedit /etc/brclio-mail/brclio-mail.env
```

至少核对：

```dotenv
BRCLIO_HOSTNAME=mail.example.com
BRCLIO_BASE_URL=https://mail.example.com
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
BRCLIO_DIRECT_DELIVERY=false
BRCLIO_DEV_MODE=false
BRCLIO_DISABLE_MAIL_SERVERS=false
```

编辑后检查 root-only 权限：

```bash
sudo chown root:root /etc/brclio-mail/brclio-mail.env
sudo chmod 0600 /etc/brclio-mail/brclio-mail.env
sudo stat -c '%U:%G %a %n' /etc/brclio-mail/brclio-mail.env
```

预期为 `root:root 600`。不要把密码写进该环境文件。

### 外发前必须配置并验证 smarthost

本文保持 `BRCLIO_DIRECT_DELIVERY=false`。如果不配置 relay，doctor 会报告 `"deliveryMode":"disabled"`，向外域发送的邮件只会失败重试；要完成公司邮箱的外发验收，必须先配置并验证 smarthost。

环境文件填写非敏感参数：

```dotenv
BRCLIO_RELAY_ADDR=smtp.provider.example:465
BRCLIO_RELAY_USERNAME=account@example.com
BRCLIO_RELAY_IMPLICIT_TLS=true
BRCLIO_DIRECT_DELIVERY=false
```

真实密码写入单独 secret：

```bash
sudoedit /etc/brclio-mail/secrets/relay_password
sudo chown root:root /etc/brclio-mail/secrets/relay_password
sudo chmod 0600 /etc/brclio-mail/secrets/relay_password
```

提供商使用 `587 + STARTTLS` 时，把地址改为 `:587`，并把 `BRCLIO_RELAY_IMPLICIT_TLS` 设为 `false`。当前 relay 认证只支持 TLS 内的 `AUTH PLAIN`，必须先向服务商确认。

## 8. 启动并查看日志

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
if ! grep -F '"deliveryMode":"smarthost"' <<<"$doctor_report"; then
  echo 'smarthost is not active; do not start external mail service' >&2
  exit 1
fi
sudo systemctl enable --now brclio-mail
sudo systemctl status brclio-mail --no-pager
sudo journalctl -u brclio-mail -n 100 --no-pager
/usr/local/bin/brclio-mail version
```

再确认监听端口：

```bash
sudo ss -ltnp | grep -E ':(25|80|443|465|587|993)\b'
curl -fsS https://mail.example.com/healthz
```

本机监听正常不代表宝塔防火墙、云安全组、IPv6 与公网证书路径已经正常。请在手机热点、另一台云主机或其他外部网络执行；把主机名替换为真实值：

```bash
for port in 25 80 443 465 587 993; do
  nc -vz mail.example.com "$port"
done
openssl s_client -starttls smtp -connect mail.example.com:25 \
  -servername mail.example.com -verify_hostname mail.example.com \
  -verify_return_error </dev/null
openssl s_client -connect mail.example.com:465 \
  -servername mail.example.com -verify_hostname mail.example.com \
  -verify_return_error </dev/null
openssl s_client -starttls smtp -connect mail.example.com:587 \
  -servername mail.example.com -verify_hostname mail.example.com \
  -verify_return_error </dev/null
openssl s_client -connect mail.example.com:993 \
  -servername mail.example.com -verify_hostname mail.example.com \
  -verify_return_error </dev/null
```

四次 TLS 检查都必须返回成功的证书验证结果；超时或验证失败时不要继续域名与真实收发验收。

若报 `address already in use`，回到第 2 节检查 Nginx、Apache、Postfix、Exim、Dovecot 或旧邮件容器。不能让两个服务共享同一公网端口。

## 9. 首次管理员初始化

读取一次性 setup token：

```bash
sudo cat /etc/brclio-mail/secrets/setup_token
```

打开 `https://mail.example.com`，用该 token 创建首位管理员。不要把 token 放进宝塔计划任务、命令截图、公开工单或聊天记录。

初始化完成后轮换 token 并重启：

```bash
openssl rand -base64 48 | sudo tee \
  /etc/brclio-mail/secrets/setup_token >/dev/null
sudo chown root:root /etc/brclio-mail/secrets/setup_token
sudo chmod 0600 /etc/brclio-mail/secrets/setup_token
sudo systemctl restart brclio-mail
```

## 10. 验证首次域并按需添加其他域

首次初始化会同时创建表单中填写的首个 `pending` 邮件域。进入后台查看 `example.com`，先发布它生成的随机验证记录；只有新增其他域时才再使用“添加域名”：

```text
_brclio-mail.example.com. TXT "<管理台生成的 token>"
```

等待生效后在后台点击检查，状态必须从 `pending` 变为 `verified`。然后完成 MX、SPF、DKIM、DMARC、TLS-RPT 和客户端发现记录，完整表格见 [DNS、PTR 与发信信誉](dns.md)。不要照抄文档中的示例 IP、smarthost SPF 或 DKIM 公钥。

## 11. 分配邮箱与第三方客户端

管理员可在后台创建用户、别名、邮箱容量和应用专用密码。推荐每台设备创建独立应用密码：

| 用途 | 主机 | 端口 | 加密 |
| --- | --- | ---: | --- |
| IMAP 收信 | `mail.example.com` | `993` | SSL/TLS |
| SMTP 发信 | `mail.example.com` | `465` | SSL/TLS |
| SMTP 备选 | `mail.example.com` | `587` | STARTTLS |

用户名始终填写完整邮箱地址。不要使用端口 25 配置邮件客户端，完整说明见 [第三方邮件客户端](clients.md)。

## 12. 用宝塔计划任务触发一致性备份

先手工执行并验收下面的 Shell。只有同时配置了本机保留期限、加密异地复制、失败告警和定期恢复验证后，才把它加入宝塔“计划任务”；否则每日快照会不断占满磁盘。

```bash
#!/usr/bin/env bash
set -Eeuo pipefail
backup_name="baota-$(date -u +%Y%m%dT%H%M%SZ)"
systemctl start "brclio-mail-backup@${backup_name}.service"
test -f "/var/backups/brclio-mail/${backup_name}.sqlite"
stat -c '%U:%G %a %s %n' "/var/backups/brclio-mail/${backup_name}.sqlite"
```

任务成功只说明本机快照生成完成。还必须把备份、`/etc/brclio-mail` 配置和必要 secret 加密复制到另一故障域，并定期做真实恢复。不要用宝塔文件管理直接复制运行中的 `.db`、`-wal`、`-shm`。

宝塔任务日志必须接入告警；本机快照只能按书面保留策略清理，并且要在确认异地副本可读后执行。教程不提供自动删除命令，避免时间、路径或恢复状态判断错误导致误删。

## 13. 升级、卸载与日常检查

升级前先创建并移出可恢复备份：

```bash
bash <<'BRCLIO_BAOTA_UPGRADE'
set -Eeuo pipefail
target_version="vX.Y.Z" # 必须替换为已审阅的新 Release tag
[[ "$target_version" != "vX.Y.Z" ]] || {
  echo 'replace target_version before upgrading' >&2
  exit 1
}
repo_dir="/root/brclio-mail"
[[ -d "$repo_dir/.git" && ! -L "$repo_dir" ]] || {
  echo 'reviewed repository checkout not found' >&2
  exit 1
}
cd "$repo_dir"
test -z "$(git status --porcelain)"
git fetch --tags
git checkout "$target_version"
test -z "$(git status --porcelain)"
test "$(git describe --tags --exact-match)" = "$target_version"
sudo ./scripts/upgrade-systemd.sh --version "$target_version"
BRCLIO_BAOTA_UPGRADE
```

现有安装不能再次直接运行 installer 覆盖；必须使用带数据库/package 快照和 doctor 门禁的 `upgrade-systemd.sh`。

日常检查：

```bash
curl -fsS https://mail.example.com/healthz
sudo systemctl is-active brclio-mail
sudo journalctl -u brclio-mail --since "30 minutes ago" --no-pager
sudo systemctl start brclio-mail-doctor.service
sudo journalctl -u brclio-mail-doctor.service -n 100 --no-pager
```

卸载入口：

```bash
cd /root/brclio-mail
sudo ./scripts/uninstall-systemd.sh
```

卸载器保留配置、SQLite、备份和服务账号，并打印恢复命令；不要手工删除这些路径，除非已经验证异地备份。

## 14. 最终验收清单

- 宝塔系统防火墙与云安全组都放行六个 TCP 端口；
- 宝塔/Nginx/Apache/Postfix 等没有抢占目标端口；
- `systemctl is-active brclio-mail` 返回 `active`；
- HTTPS 证书覆盖邮件主机名，外部 `/healthz` 正常；
- A、PTR、MX、验证 TXT、SPF、DKIM、DMARC 查询结果正确；
- 管理台域名为 `verified`；
- 完成一次真实外部收信、外部发信和第三方客户端收发；
- 普通用户删除邮件后不可见，但管理员归档仍可按原因审计访问；
- 完成一次一致性备份并在隔离环境验证；
- 已向所有用户披露管理员能够读取往来邮件及已从用户视图删除的邮件。

## 宝塔官方参考

- [宝塔快速安装与跳过 LNMP/LAMP](https://docs.bt.cn/getting-started/quick-installation-of-bt-panel)
- [宝塔在线 SSH 终端与 Web Shell](https://docs.bt.cn/user-guide/xterm/)
- [宝塔系统防火墙端口规则](https://docs.bt.cn/user-guide/security/firewall/port-rule)
- [宝塔计划任务与 Shell 脚本](https://docs.bt.cn/category/%E8%AE%A1%E5%88%92%E4%BB%BB%E5%8A%A1)
- [宝塔服务器安全配置](https://docs.bt.cn/user-guide/security/server-safe/security-config)
- [宝塔面板端口设置](https://docs.bt.cn/getting-started/edit-panel-port)
- [宝塔命令行工具](https://docs.bt.cn/getting-started/bt-command-line-tool)
- [宝塔反向代理说明](https://docs.bt.cn/user-guide/site/php/site-config/reverse-proxy)
- [宝塔云安全组放行说明](https://docs.bt.cn/getting-started/allow-panel-port-access)

继续阅读：[命令行与一键部署](tutorial-command-line.md) · [Docker Compose 部署](tutorial-docker.md) · [运维、备份与恢复](operations.md)
