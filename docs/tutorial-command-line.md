# 命令行部署 Brclio Mail：一键安装与完整手动教程

本文适用于不使用服务器面板、希望通过 SSH 直接部署的个人或小公司管理员，包含两条路线：

1. **一键部署**：一次粘贴完成系统依赖、明确 tag 下载、Release 校验和 systemd 安装；安全起见默认不自动启动。
2. **完整手动部署**：逐项检查端口、配置、secret、TLS 与服务状态，适合正式服务器。

两条路线最终都使用同一个 `install-systemd.sh`，不会安装 Docker，也不会把邮件服务作为 root 运行。

![Bornforthis 拉下一键安装总闸并逐项点亮检查灯](../assets/command-line-deployment-illustrations/01-one-paste-install-lever.png)

> 当前 `v0.2.1-preview` 是单节点 Preview。不要用于唯一副本、关键业务、高可用或合规归档；上线前阅读[限制与路线图](limitations-roadmap.md)。

> **公司使用前先确认许可。** 后端代码按 AGPL 提供；当前完整界面包含 Esther Design System 衍生内容，仓库将该部分标记为 CC BY-NC-SA 4.0 非商业许可。商业经营场景必须替换相关 UI/设计或取得单独授权，并履行 AGPL 的网络源代码义务。

## 1. 你需要准备什么

| 项目 | 要求 |
| --- | --- |
| Linux | 64 位 Ubuntu/Debian 或 RHEL/Rocky/AlmaLinux |
| init | `systemd 247` 或更新版本 |
| CPU | `amd64/x86_64` 或 `arm64/aarch64` |
| 存储 | 本机块存储，不能是 NFS/SMB/CephFS/GlusterFS/FUSE |
| 网络 | 固定公网 IP，可设置 PTR，开放 TCP `25/80/443/465/587/993` |
| 域名 | 邮件主机名，例如 `mail.example.com` |
| 出站 | 推荐准备 authenticated smarthost，直接 MX 投递保持关闭 |

仓库的真实 systemd 集成 CI 当前运行在 Ubuntu；RHEL/Rocky/AlmaLinux 的依赖命令与 systemd 路径已经文档化，但尚未达到同等级真实 runner 验收。RHEL 系正式使用前应先在同版本测试机演练 SELinux、安装、备份、升级和回滚。

先在 DNS 控制台创建：

```text
mail.example.com.  A    203.0.113.10
203.0.113.10       PTR  mail.example.com.
```

PTR 在公网 IP 所属云厂商设置。不要给邮件主机开启 CDN/HTTP 代理；ACME HTTP-01、SMTP 和 IMAP 必须直接到达源服务器。只有 IPv6 的六个服务端口都能端到端到达时才发布 AAAA，错误的 AAAA 会让部分客户端和远端邮件服务器优先走不可达路径。

为减少一次粘贴时的输入错误，本文一键块只接受常规 ASCII ACME 邮箱，不接受 `&`、反斜线或引号；安装器本身会转义 env 模板的替换字符，并有对应包验证测试。一键块会在安装任何软件包之前完成发行版、CPU、systemd 与邮箱格式预检。

同时在云安全组与主机防火墙放行六个 TCP 端口。若服务器已有 Nginx、Apache、Postfix、Exim 或 Dovecot，先确认不会冲突。

## 2. 部署前只读检查

```bash
uname -s
uname -m
systemctl --version | head -1
df -h / /var /tmp
findmnt --target /var
sudo ss -ltnp | grep -E ':(25|80|443|465|587|993)\b' || true
```

预期：Linux、受支持架构、systemd 版本不低于 247、数据盘空间充足，并且目标端口没有被其他程序监听。

## 3. 一键部署：一次粘贴完成依赖与安装

先完整阅读下面的脚本，把最前面的三个值替换为真实值。它不采用 `curl | bash`，而是从 GitHub 检出明确 tag，再由仓库安装器下载同版本发行包、核对 SHA-256 和二进制版本。

```bash
bash <<'BRCLIO_ONE_CLICK'
set -Eeuo pipefail

target_version='v0.2.1-preview'
mail_hostname='mail.example.com'
acme_email='postmaster@example.com'

[[ "$(uname -s)" == 'Linux' ]] || {
  echo '一键安装只支持 Linux' >&2
  exit 1
}
case "$(uname -m)" in
  x86_64 | aarch64) ;;
  *) echo '只支持 x86_64/amd64 或 aarch64/arm64' >&2; exit 1 ;;
esac
[[ -r /etc/os-release ]] || {
  echo '无法识别 Linux 发行版' >&2
  exit 1
}
. /etc/os-release
case "${ID:-}" in
  ubuntu | debian) package_family='apt' ;;
  rhel | rocky | almalinux) package_family='dnf' ;;
  *) echo '一键路线只支持 Ubuntu/Debian/RHEL/Rocky/AlmaLinux' >&2; exit 1 ;;
esac
command -v systemctl >/dev/null || {
  echo '找不到 systemctl' >&2
  exit 1
}
read -r _ systemd_version _ < <(systemctl --version)
[[ "$systemd_version" =~ ^[0-9]+$ && "$systemd_version" -ge 247 ]] || {
  printf '需要 systemd 247+，当前为 %s\n' "$systemd_version" >&2
  exit 1
}
[[ "$acme_email" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || {
  echo 'ACME 邮箱只使用常规 ASCII 地址，不要包含 &、反斜线或引号' >&2
  exit 1
}

if [ "$(id -u)" -eq 0 ]; then
  root_cmd=()
else
  command -v sudo >/dev/null || {
    echo '需要 root 或 sudo 权限' >&2
    exit 1
  }
  root_cmd=(sudo)
fi

if [[ "$package_family" == 'apt' ]] && command -v apt-get >/dev/null; then
  "${root_cmd[@]}" apt-get update
  "${root_cmd[@]}" apt-get install -y \
    bash git curl ca-certificates openssl tar coreutils util-linux \
    passwd sqlite3 grep sed findutils gawk iproute2 netcat-openbsd
elif [[ "$package_family" == 'dnf' ]] && command -v dnf >/dev/null; then
  "${root_cmd[@]}" dnf install -y \
    bash git curl ca-certificates openssl tar coreutils util-linux \
    shadow-utils sqlite grep sed findutils gawk iproute nmap-ncat
else
  echo '当前发行版缺少预期包管理器；请改用本文的手动部署路线' >&2
  exit 1
fi

install_workspace="$(mktemp -d /tmp/brclio-mail-bootstrap.XXXXXX)"
trap 'rm -rf -- "${install_workspace}"' EXIT

git clone --branch "${target_version}" --depth 1 \
  https://github.com/Brclio/brclio-mail.git \
  "${install_workspace}/brclio-mail"

cd "${install_workspace}/brclio-mail"
git status --short
test "$(git describe --tags --exact-match)" = "${target_version}"

"${root_cmd[@]}" ./scripts/install-systemd.sh \
  --version "${target_version}" \
  --hostname "${mail_hostname}" \
  --acme-email "${acme_email}" \
  --no-start

echo '安装文件已经完成；请检查 /etc/brclio-mail/brclio-mail.env 后再启动。'
BRCLIO_ONE_CLICK
```

这段“一键部署”有意保留一个人工启动门禁：它会安装软件、配置模板、secret、服务用户与 systemd unit，但不会擅自修改云安全组、DNS、PTR、SELinux 或启动公网监听器。

检查配置后启动：

```bash
sudoedit /etc/brclio-mail/brclio-mail.env
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
  echo 'configure smarthost before external mail service' >&2
  exit 1
fi
sudo systemctl enable --now brclio-mail
sudo systemctl status brclio-mail --no-pager
```

教程始终保留 `--no-start`，因为安装器不能替你核验云安全组、PTR、真实 DNS、端口占用、SELinux 与外部收发。完成 doctor 和人工检查后再显式启动，才是这里“一键安装”的安全边界。

## 4. 完整手动部署

### 4.1 安装依赖

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

### 4.2 检出明确版本

```bash
set -Eeuo pipefail
target_version="v0.2.1-preview"
repo_dir="${PWD}/brclio-mail"
[[ ! -e "$repo_dir" && ! -L "$repo_dir" ]] || {
  echo 'target checkout already exists; inspect it instead of reusing it' >&2
  exit 1
}
git clone --branch "$target_version" --depth 1 \
  https://github.com/Brclio/brclio-mail.git "$repo_dir"
cd "$repo_dir"
test -z "$(git status --porcelain)"
git log -1 --oneline
test "$(git describe --tags --exact-match)" = "$target_version"
```

### 4.3 暂存 systemd 安装

```bash
sudo ./scripts/install-systemd.sh \
  --version "$target_version" \
  --hostname mail.example.com \
  --acme-email postmaster@example.com \
  --no-start
```

安装器会检查：

- Linux、root、systemd 版本和 CPU 架构；
- Release 包与 `checksums.txt` 中的 SHA-256；
- 二进制产品名和版本；
- 敏感文件必须是 root 所有、`0600`、非符号链接；
- SQLite 数据路径不是网络/FUSE 文件系统；
- 现有安装不能被直接覆盖；
- 服务以 `brclio-mail` 用户运行，仅获得绑定低端口所需的能力。

Release 校验和与包来自同一 GitHub Release，目前没有单独的制品签名或 attestation；上线前仍应审阅 tag、发布记录和安装脚本。

### 4.4 检查主配置

```bash
sudoedit /etc/brclio-mail/brclio-mail.env
```

基础配置：

```dotenv
BRCLIO_HOSTNAME=mail.example.com
BRCLIO_BASE_URL=https://mail.example.com
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com
BRCLIO_DIRECT_DELIVERY=false
BRCLIO_DEV_MODE=false
BRCLIO_DISABLE_MAIL_SERVERS=false
```

不要更改 systemd 默认的：

```dotenv
BRCLIO_DATA_DIR=/var/lib/brclio-mail
BRCLIO_DATABASE_PATH=/var/lib/brclio-mail/brclio-mail.db
```

受支持的事务式升级脚本依赖这两个标准路径。SQLite 必须保持单机、单服务实例。

### 4.5 外发前配置 smarthost

本教程保持 `BRCLIO_DIRECT_DELIVERY=false`；不配置 relay 时 doctor 会报告 `"deliveryMode":"disabled"`，外域邮件不会真正发出。完整收发上线必须先完成本节。

环境文件：

```dotenv
BRCLIO_RELAY_ADDR=smtp.provider.example:465
BRCLIO_RELAY_USERNAME=account@example.com
BRCLIO_RELAY_IMPLICIT_TLS=true
BRCLIO_DIRECT_DELIVERY=false
```

密码只写入 secret：

```bash
sudoedit /etc/brclio-mail/secrets/relay_password
sudo chown root:root /etc/brclio-mail/secrets/relay_password
sudo chmod 0600 /etc/brclio-mail/secrets/relay_password
```

使用 `587 + STARTTLS` 时改为 `:587` 和 `false`。当前只支持 TLS 内的 SASL `AUTH PLAIN`，不支持 LOGIN 或 OAuth2。

### 4.6 启动服务

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
  echo 'configure smarthost before external mail service' >&2
  exit 1
fi
sudo systemctl enable --now brclio-mail
sudo systemctl is-active brclio-mail
sudo systemctl status brclio-mail --no-pager
sudo journalctl -u brclio-mail -n 100 --no-pager
/usr/local/bin/brclio-mail version
```

![Bornforthis 沿着预检、安装、配置、启动路径逐站检查](../assets/command-line-deployment-illustrations/02-preflight-to-service-route.png)

## 5. 首次初始化与域名验证

读取 setup token：

```bash
sudo cat /etc/brclio-mail/secrets/setup_token
```

浏览 `https://mail.example.com` 创建首位管理员，随后轮换 token：

```bash
openssl rand -base64 48 | sudo tee \
  /etc/brclio-mail/secrets/setup_token >/dev/null
sudo chown root:root /etc/brclio-mail/secrets/setup_token
sudo chmod 0600 /etc/brclio-mail/secrets/setup_token
sudo systemctl restart brclio-mail
```

首次初始化会在同一事务中创建填写的首个 `pending` 邮件域。进入后台查看该域并发布 `_brclio-mail.<domain>` TXT，直到状态为 `verified`；只有新增其他域时才再使用“添加域名”。随后按 [DNS 文档](dns.md)完成 MX、SPF、DKIM、DMARC、TLS-RPT 和 SRV。

## 6. 邮箱分配与第三方客户端

管理员可创建用户、别名、邮箱容量与应用专用密码。客户端参数：

| 用途 | 主机 | 端口 | 安全方式 |
| --- | --- | ---: | --- |
| IMAP 收信 | `mail.example.com` | `993` | SSL/TLS |
| SMTP 发信 | `mail.example.com` | `465` | SSL/TLS |
| SMTP 备选 | `mail.example.com` | `587` | STARTTLS |

用户名使用完整邮箱地址。每台设备单独创建应用密码，详情见[第三方邮件客户端](clients.md)。

## 7. 从外部完成验收

不要只在服务器本机测试：

```bash
curl -fsS https://mail.example.com/healthz
openssl s_client -connect mail.example.com:443 \
  -servername mail.example.com -verify_hostname mail.example.com \
  -verify_return_error </dev/null
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
nc -vz mail.example.com 25
```

然后完成真实外部收信、外部发信、IMAP 同步、队列与管理员归档检查。DNS 正确不等于一定能进入收件箱，仍要观察收件方的 `Authentication-Results` 与发信信誉。

## 8. 备份、升级与卸载

一致性在线备份：

```bash
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_name="manual-${stamp}"
sudo systemctl start "brclio-mail-backup@${backup_name}.service"
sudo stat -c '%U:%G %a %s %n' \
  "/var/backups/brclio-mail/${backup_name}.sqlite"
```

不要 `cp` 运行中的 `.db`、`-wal`、`-shm`。本机快照还必须加密复制到另一故障域并完成恢复演练。

升级。无论最初走一键还是手动路线，都重新检出明确的新 tag；这样不依赖一键安装结束后已自动清理的临时源码目录：

```bash
bash <<'BRCLIO_UPGRADE'
set -Eeuo pipefail
target_version='vX.Y.Z' # 必须替换为已审阅的新 Release tag
[[ "$target_version" != 'vX.Y.Z' ]] || {
  echo 'replace target_version before upgrading' >&2
  exit 1
}

upgrade_workspace="$(mktemp -d /tmp/brclio-mail-upgrade.XXXXXX)"
trap 'rm -rf -- "${upgrade_workspace}"' EXIT
git clone --branch "$target_version" --depth 1 \
  https://github.com/Brclio/brclio-mail.git \
  "${upgrade_workspace}/brclio-mail"
cd "${upgrade_workspace}/brclio-mail"
test "$(git describe --tags --exact-match)" = "$target_version"
sudo ./scripts/upgrade-systemd.sh --version "$target_version"
/usr/local/bin/brclio-mail version
BRCLIO_UPGRADE
```

升级脚本只接受一个明确目标：`--version TAG` 或 `--binary PATH`。它会在停服前预取目标，随后创建匹配的数据库和 package 快照、运行新版本 doctor，并在监听器开放前的失败阶段执行成对恢复。

卸载同样重新取得与已安装版本匹配、已经审阅的脚本；卸载器默认保留数据：

```bash
bash <<'BRCLIO_UNINSTALL'
set -Eeuo pipefail
installed_version='v0.2.1-preview' # 替换为当前已安装 Release tag

uninstall_workspace="$(mktemp -d /tmp/brclio-mail-uninstall.XXXXXX)"
trap 'rm -rf -- "${uninstall_workspace}"' EXIT
git clone --branch "$installed_version" --depth 1 \
  https://github.com/Brclio/brclio-mail.git \
  "${uninstall_workspace}/brclio-mail"
cd "${uninstall_workspace}/brclio-mail"
test "$(git describe --tags --exact-match)" = "$installed_version"
sudo ./scripts/uninstall-systemd.sh
BRCLIO_UNINSTALL
```

卸载默认保留 `/etc/brclio-mail`、`/var/lib/brclio-mail`、`/var/backups/brclio-mail` 和服务账号。

## 9. 常见失败

### `address already in use`

```bash
sudo ss -ltnp | grep -E ':(25|80|443|465|587|993)\b'
```

找到并确认冲突服务，不要直接杀死未知 PID。

### ACME 申请失败

检查 A/AAAA、80/443 云安全组、本机防火墙、CDN 代理和是否有另一 Web 服务拦截 `/.well-known/acme-challenge/`。

### 外部收不到邮件

确认云厂商允许入站 25、MX 指向有 A/AAAA 的主机名、管理台域名已 `verified`，并查看 journal。

### 能收不能发

检查 smarthost 地址、用户名、secret、465/587 TLS 模式和 SPF 是否授权真实出口。直接 MX 投递默认关闭且不推荐。

### SQLite/doctor 报错

停止写入并保留现场，不要删除 WAL 或直接修改数据库。按[运维、备份与恢复](operations.md)使用一致性快照处理。

继续阅读：[宝塔面板部署](tutorial-baota.md) · [Docker Compose 部署](tutorial-docker.md) · [部署与 TLS 参考](deployment.md)
