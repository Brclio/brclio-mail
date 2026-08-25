# 宝塔部署 Brclio Mail：一页快速版

这是给个人和小公司的**简明流程**：不用 Docker，不创建宝塔“网站”，只用宝塔的“终端”和“系统防火墙”。上线主体只有三段命令：**安装 → 配置中继 → 检查并启动**。

适用前提：独立 Linux VPS、固定公网 IP、`amd64`/`arm64` CPU、systemd 247 或更新版本，且 `80/443` 没有被 Nginx、Apache 或其他网站占用。当前仍是 Preview，不要把它作为关键邮件的唯一副本。

![Bornforthis 把宝塔操作台接到 systemd 邮件机](../assets/baota-deployment-illustrations/01-panel-controls-systemd.png)

> 遇到端口冲突、静态证书、SELinux、升级或恢复时，使用[宝塔完整版](tutorial-baota.md)。

## 流程总览

1. 配置 DNS；
2. 宝塔和云安全组放行端口；
3. 在宝塔终端粘贴安装命令；
4. 粘贴 SMTP 中继配置；
5. 检查并启动；
6. 浏览器创建域和邮箱；
7. 邮件客户端登录并完成外网收发测试。

## 1. DNS 和端口

先准备这些信息：

| 信息 | 示例 |
| --- | --- |
| 邮件主机名 | `mail.example.com` |
| 邮箱域名 | `example.com`，员工地址会是 `name@example.com` |
| 服务器公网 IP | `203.0.113.10` |
| ACME 联系邮箱 | `postmaster@example.com` |
| SMTP 中继 | 地址、账号、密码 |

在域名服务商添加 A 记录：

```text
mail.example.com  ->  203.0.113.10
```

在 VPS/云厂商添加 PTR（反向 DNS）：

```text
203.0.113.10  ->  mail.example.com
```

不要给 `mail.example.com` 开 CDN/代理。没有完成 IPv6 验收时先不要添加 AAAA。

然后在宝塔“安全” → “系统防火墙”和云厂商安全组中放行 TCP：

```text
25, 80, 443, 465, 587, 993
```

还要向云厂商确认**入站 TCP 25 没有被上游封禁**，只看安全组不够。宝塔面板和 SSH 端口只对你的管理 IP 开放；不要公开数据库、phpMyAdmin 或 FTP。

## 2. 第一段：安装

打开宝塔“终端”。如果当前不是 root，先执行 `sudo -i`。把 `mail_hostname` 和 `acme_email` 改成自己的值，再整段粘贴：

```bash
bash <<'BRCLIO_INSTALL'
set -Eeuo pipefail
target_version='v0.2.1-preview'
mail_hostname='mail.example.com'
acme_email='postmaster@example.com'

[[ "$(id -u)" -eq 0 ]] || { echo '请先执行 sudo -i' >&2; exit 1; }
[[ "$mail_hostname" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || {
  echo 'mail_hostname 格式不正确' >&2; exit 1;
}
[[ "$acme_email" =~ ^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || {
  echo 'acme_email 格式不正确' >&2; exit 1;
}
systemd_version="$(systemctl --version | awk 'NR == 1 { print $2 }')"
[[ "$systemd_version" =~ ^[0-9]+$ && "$systemd_version" -ge 247 ]] || {
  echo '需要 systemd 247 或更新版本' >&2; exit 1;
}
case "$(uname -m)" in
  x86_64|amd64|aarch64|arm64) ;;
  *) echo '只支持 amd64 或 arm64 CPU' >&2; exit 1 ;;
esac
if command -v apt-get >/dev/null; then
  apt-get update
  apt-get install -y bash git curl ca-certificates openssl tar coreutils \
    util-linux passwd sqlite3 grep sed findutils gawk iproute2 netcat-openbsd
elif command -v dnf >/dev/null; then
  dnf install -y bash git curl ca-certificates openssl tar coreutils \
    util-linux shadow-utils sqlite grep sed findutils gawk iproute nmap-ncat
else
  echo '仅支持 Ubuntu/Debian/RHEL/Rocky/AlmaLinux' >&2
  exit 1
fi

repo_dir='/root/brclio-mail'
[[ ! -e "$repo_dir" && ! -L "$repo_dir" ]] || {
  echo '/root/brclio-mail 已存在，请改用宝塔完整版处理' >&2
  exit 1
}
git clone --branch "$target_version" --depth 1 \
  https://github.com/Brclio/brclio-mail.git "$repo_dir"
cd "$repo_dir"
test -z "$(git status --porcelain)"
test "$(git describe --tags --exact-match)" = "$target_version"
./scripts/install-systemd.sh --version "$target_version" \
  --hostname "$mail_hostname" --acme-email "$acme_email" --no-start
echo '安装完成，服务尚未启动。'
BRCLIO_INSTALL
```

看到“安装完成，服务尚未启动”再继续。

## 3. 第二段：配置 SMTP 中继

外发邮件推荐通过 smarthost。把前三行改成中继服务商给你的参数；465 用 `true`，587 + STARTTLS 用 `false`：

```bash
bash <<'BRCLIO_RELAY'
set -Eeuo pipefail
relay_addr='smtp.provider.example:465'
relay_user='account@example.com'
relay_implicit_tls='true'

[[ "$relay_addr" != 'smtp.provider.example:465' && \
   "$relay_addr" =~ ^[A-Za-z0-9.-]+:[0-9]{2,5}$ ]] || {
  echo 'relay_addr 应为 smtp.example.com:465' >&2; exit 1;
}
[[ "$relay_user" != 'account@example.com' && \
   "$relay_user" =~ ^[A-Za-z0-9._%+@=-]+$ ]] || {
  echo '请填写真实 relay_user' >&2; exit 1;
}
[[ "$relay_implicit_tls" == true || "$relay_implicit_tls" == false ]] || {
  echo 'relay_implicit_tls 只能是 true 或 false' >&2; exit 1;
}

env_file='/etc/brclio-mail/brclio-mail.env'
secret_file='/etc/brclio-mail/secrets/relay_password'
[[ -f "$env_file" && ! -L "$env_file" && \
   -f "$secret_file" && ! -L "$secret_file" ]]
sed -i \
  -e "s|^BRCLIO_RELAY_ADDR=.*|BRCLIO_RELAY_ADDR=${relay_addr}|" \
  -e "s|^BRCLIO_RELAY_USERNAME=.*|BRCLIO_RELAY_USERNAME=${relay_user}|" \
  -e "s|^BRCLIO_RELAY_IMPLICIT_TLS=.*|BRCLIO_RELAY_IMPLICIT_TLS=${relay_implicit_tls}|" \
  -e 's|^BRCLIO_DIRECT_DELIVERY=.*|BRCLIO_DIRECT_DELIVERY=false|' \
  "$env_file"

[[ -r /dev/tty ]] || { echo '当前终端不可交互，请改用宝塔完整版' >&2; exit 1; }
IFS= read -r -s -p '输入 SMTP 中继密码: ' relay_password </dev/tty
printf '\n'
[[ -n "$relay_password" ]]
umask 077
printf '%s' "$relay_password" >"$secret_file"
unset relay_password
chown root:root "$env_file" "$secret_file"
chmod 0600 "$env_file" "$secret_file"
BRCLIO_RELAY
```

密码输入时屏幕不会显示字符，这是正常的。不要把密码写进命令、`.env`、截图或工单。中继服务商必须支持 TLS 内的 SMTP `AUTH PLAIN`。

## 4. 第三段：检查并启动

整段粘贴。doctor 失败或未确认 smarthost 时，命令会停止，不会开启邮件服务：

```bash
bash <<'BRCLIO_START'
set -Eeuo pipefail
systemctl daemon-reload
systemctl start brclio-mail-doctor.service || {
  journalctl -u brclio-mail-doctor.service -n 100 --no-pager
  exit 1
}
doctor_id="$(systemctl show brclio-mail-doctor.service \
  --property=InvocationID --value)"
[[ -n "$doctor_id" ]]
doctor_report="$(journalctl "_SYSTEMD_INVOCATION_ID=${doctor_id}" \
  --no-pager -o cat)"
printf '%s\n' "$doctor_report"
grep -F '"deliveryMode":"smarthost"' <<<"$doctor_report"
systemctl enable --now brclio-mail.service
systemctl is-active --quiet brclio-mail.service
systemctl status brclio-mail.service --no-pager
BRCLIO_START
```

最后看到 `active (running)` 即启动成功。失败时运行：

```bash
journalctl -u brclio-mail.service -n 100 --no-pager
```

## 5. 创建域和邮箱

1. 在宝塔终端运行 `cat /etc/brclio-mail/secrets/setup_token`；
2. 打开 `https://mail.example.com`，用 token 创建首位管理员；邮件域填写 `example.com`，不要误填成服务器主机名 `mail.example.com`；
3. 按管理台提示发布 `_brclio-mail` 验证 TXT，直到域状态变成 `verified`；
4. 发布管理台给出的 DKIM，并按 [DNS 文档](dns.md)配置 MX、SPF 和 DMARC；
5. 在“用户/邮箱”中创建员工邮箱、设置容量，并创建应用专用密码。

完成后轮换一次 setup token：

```bash
set -Eeuo pipefail
token_dir='/etc/brclio-mail/secrets'
token_file="${token_dir}/setup_token"
[[ -d "$token_dir" && ! -L "$token_dir" && \
   -f "$token_file" && ! -L "$token_file" ]]
umask 077
token_tmp="$(mktemp "${token_dir}/.setup_token.XXXXXX")"
trap 'rm -f -- "$token_tmp"' EXIT
openssl rand -base64 48 >"$token_tmp"
[[ -s "$token_tmp" ]]
chown root:root "$token_tmp"
chmod 0600 "$token_tmp"
mv -f -- "$token_tmp" "$token_file"
trap - EXIT
systemctl restart brclio-mail.service
```

## 6. 登录邮件客户端

| 用途 | 服务器 | 端口 | 加密 | 账号 |
| --- | --- | ---: | --- | --- |
| 收件 IMAP | `mail.example.com` | `993` | SSL/TLS | 完整邮箱地址 |
| 发件 SMTP | `mail.example.com` | `465` | SSL/TLS | 完整邮箱地址 |
| 发件 SMTP | `mail.example.com` | `587` | STARTTLS | 完整邮箱地址 |

密码使用管理台生成的**应用专用密码**，不是 Web 登录密码。

## 7. 完成判定

用手机热点或另一台云服务器测试，以下项目必须全部通过：

- `https://mail.example.com` 可打开且证书正确；
- 外部邮箱能发到公司邮箱；
- 公司邮箱能发到外部邮箱；
- IMAP 能同步，465 或 587 能发信；
- 宝塔面板、SSH 和数据库端口没有向全网公开。

再生成第一份一致性备份：

```bash
set -Eeuo pipefail
backup_name="manual-$(date -u +%Y%m%dT%H%M%SZ)"
systemctl start "brclio-mail-backup@${backup_name}.service"
ls -lh "/var/backups/brclio-mail/${backup_name}.sqlite"
```

把备份加密后复制到另一台机器或对象存储。自动备份、升级和恢复见[宝塔完整版](tutorial-baota.md)。

## 上线前必须知道

- 当前是单机 Preview，不是高可用或合规归档系统，且尚未内置反垃圾、杀毒及入站 SPF/DKIM/DMARC 验证；
- 管理员能读取所有往来邮件，包括用户自行删除的邮件；公司必须提前披露，并遵守适用的隐私、劳动和通信法律；
- 后端采用 AGPL。Esther 衍生 UI 在仓库中标记为 CC BY-NC-SA 4.0、禁止商用；营利性公司使用前必须替换该设计层或取得授权，并履行 AGPL 网络源代码义务。
