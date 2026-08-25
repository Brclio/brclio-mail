# Docker Compose 部署 Brclio Mail：独立容器教程

本文面向已经拥有 Docker Engine 与 Compose v2 运维体系的用户。Docker 是 Brclio Mail 的**可选部署方式**；如果服务器不能运行 Docker，请使用[命令行/systemd 教程](tutorial-command-line.md)或[宝塔快速部署教程](tutorial-baota-quick.md)。

Compose 方案仍然是单机、单容器、单 SQLite volume，不会因为放进 Docker 就自动获得高可用、跨主机容灾或合规归档能力。

![Bornforthis 把邮件服务装进单机容器并接好六个端口](../assets/docker-deployment-illustrations/01-single-host-mail-container.png)

> 当前版本为 `v0.2.1-preview`。不要把它作为关键邮件的唯一副本；上线前阅读[限制与路线图](limitations-roadmap.md)。

> **公司使用前先确认许可。** 后端代码按 AGPL 提供；当前完整镜像包含 Esther Design System 衍生界面，仓库将该部分标记为 CC BY-NC-SA 4.0 非商业许可。商业经营场景必须替换相关 UI/设计或取得单独授权，并履行 AGPL 的网络源代码义务。

## 1. 最终结构

仓库的 `docker-compose.yml` 会构建一个非 root、只读根文件系统的容器：

```text
主机 80   -> 容器 8080   Web/ACME
主机 443  -> 容器 8443   HTTPS
主机 25   -> 容器 2525   SMTP
主机 465  -> 容器 2465   SMTPS
主机 587  -> 容器 2587   Submission
主机 993  -> 容器 2993   IMAPS

brclio-mail-data named volume -> /data -> SQLite + ACME cache + 容器内备份
./secrets                    -> setup/relay/TLS 只读输入
```

服务进程以 UID/GID `10001:10001` 运行，丢弃全部 Linux capabilities，并启用 `no-new-privileges`。主机标准端口由 Docker 负责发布。

## 2. 前置条件

- 64 位 Linux 与本机块存储；
- Docker Engine 和 `docker compose` v2 插件；
- 主机上的 `sqlite3` CLI 与 `findmnt`（通常来自 `util-linux`），用于备份、恢复和数据卷校验；
- 固定公网 IP、可设置 PTR 的云账号；
- TCP `25/80/443/465/587/993` 未被其他程序占用；
- 云安全组和主机边界策略允许这些端口；
- 邮件主机名，例如 `mail.example.com`；
- 推荐准备 smarthost，保持直接 MX 投递关闭。

除 Docker 本身外，安装本文使用的主机工具。Ubuntu/Debian：

```bash
sudo apt-get update
sudo apt-get install -y \
  git curl ca-certificates openssl sqlite3 util-linux iproute2 \
  coreutils netcat-openbsd grep sed
```

RHEL/Rocky/AlmaLinux：

```bash
sudo dnf install -y \
  git curl ca-certificates openssl sqlite util-linux iproute \
  coreutils nmap-ncat grep sed
```

在继续前 fail-fast 检查正文依赖：

```bash
for command_name in docker git curl openssl sqlite3 findmnt ss \
  sha256sum stat install cut nc grep sed; do
  command -v "$command_name" >/dev/null || {
    printf 'missing required command: %s\n' "$command_name" >&2
    exit 1
  }
done
```

本文假设使用 rootful Docker。rootless Docker、SELinux enforcing 下的 bind-mounted secret 标签，以及 arm64 容器运行尚未经过仓库端到端 CI 验证。当前 GitHub Release 只发布 Linux 二进制 tar，没有 GHCR/Docker Hub 镜像，因此必须从明确 tag 本地构建，不能使用 `docker compose pull` 代替。Compose 服务设置了 `pull_policy: never`：运行镜像不存在时必须失败，不能静默从同名远端仓库拉取；`docker compose build --pull` 只用于刷新 Dockerfile 的受审阅基础镜像。行为定义见 Docker 官方 [`pull_policy`](https://docs.docker.com/reference/compose-file/services/#pull_policy) 与 [Compose build 规则](https://docs.docker.com/reference/compose-file/build/)。

验证 Docker：

```bash
sudo docker version
sudo docker compose version
sudo docker run --rm hello-world
```

Docker 官方建议在生产 Linux 上通过其软件仓库安装 Engine 与 Compose 插件，不建议用 convenience script 管理生产版本。Ubuntu 示例见 [Docker Engine 官方安装文档](https://docs.docker.com/engine/install/ubuntu/)；其他发行版从 [Docker Engine 安装索引](https://docs.docker.com/engine/install/)进入，Compose v2 插件见 [Compose plugin 安装文档](https://docs.docker.com/compose/install/linux/)。

## 3. 检查端口与磁盘

```bash
df -h / /var/lib/docker
sudo docker info --format '{{.DockerRootDir}}'
sudo ss -ltnp | grep -E ':(25|80|443|465|587|993)\b' || true
```

目标端口有输出时，先确认占用者。不要同时运行 systemd 版 Brclio Mail、旧邮件服务器和 Compose 版，也不要让多个容器共享同一个 SQLite volume。

## 4. 检出明确版本

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

仓库实际 Compose 文件名是 `docker-compose.yml`。Compose v2 默认会识别它；本文在关键命令中显式传入 `-f docker-compose.yml`，避免在错误目录操作另一份 Compose 项目。

## 5. 创建配置与 secret

```bash
cp .env.example .env
chmod 0600 .env

mkdir -p secrets/tls
chmod 0700 secrets secrets/tls

openssl rand -base64 36 > secrets/setup_token
: > secrets/relay_password

chmod 0600 secrets/setup_token secrets/relay_password
sudo chown -R 10001:10001 secrets
```

`.env` 只保存非敏感配置；setup token、relay 密码和静态 TLS 私钥不能提交 Git：

```bash
git status --short --ignored
```

### 5.1 自动 ACME TLS

编辑 `.env`：

```dotenv
TZ=Asia/Shanghai
BRCLIO_IMAGE=brclio-mail:0.2.1-preview
BRCLIO_VERSION=0.2.1-preview

BRCLIO_HOSTNAME=mail.example.com
BRCLIO_BASE_URL=https://mail.example.com
BRCLIO_AUTO_TLS=true
BRCLIO_ACME_EMAIL=postmaster@example.com

BRCLIO_TLS_CERT=
BRCLIO_TLS_KEY=
BRCLIO_DIRECT_DELIVERY=false
```

先让 `mail.example.com` 的 A 记录指向服务器公网 IP，并确保 80/443 直接到达这台主机。不要开启 CDN 代理。

### 5.2 使用静态证书

把完整证书链与私钥的真实文件复制到 `secrets/tls/`；不要直接引用 `/etc/letsencrypt/live/` 等外部符号链接：

```bash
sudo install -o 10001 -g 10001 -m 0600 \
  /path/to/fullchain.pem secrets/tls/fullchain.pem
sudo install -o 10001 -g 10001 -m 0600 \
  /path/to/privkey.pem secrets/tls/privkey.pem
```

在写入 `.env` 前验证文件不是符号链接、容器用户可读、证书主机名和有效期正确，且证书公钥与私钥匹配：

```bash
set -Eeuo pipefail
cert_file="secrets/tls/fullchain.pem"
key_file="secrets/tls/privkey.pem"
for file in "$cert_file" "$key_file"; do
  sudo test -f "$file"
  if sudo test -L "$file"; then
    printf 'TLS file must not be a symbolic link: %s\n' "$file" >&2
    exit 1
  fi
  test "$(sudo stat -c '%u:%g %a' "$file")" = '10001:10001 600'
done
sudo openssl x509 -in "$cert_file" -noout \
  -subject -issuer -dates -checkhost mail.example.com
sudo openssl x509 -in "$cert_file" -checkend 604800 -noout
if sudo grep -Eq 'BEGIN ENCRYPTED PRIVATE KEY|Proc-Type: 4,ENCRYPTED' \
  "$key_file"; then
  echo 'encrypted private keys are not supported by unattended startup' >&2
  exit 1
fi
sudo openssl pkey -in "$key_file" -passin pass: -check -noout
cert_spki="$(sudo openssl x509 -in "$cert_file" -pubkey -noout | \
  openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1)"
key_spki="$(sudo openssl pkey -in "$key_file" -passin pass: \
  -pubout -outform DER | \
  sha256sum | cut -d' ' -f1)"
test "$cert_spki" = "$key_spki"
```

把 `mail.example.com` 换成真实主机名。`-checkend 604800` 要求证书至少还有 7 天有效期；服务只支持无需口令的 PKCS#1、PKCS#8 或 SEC1 私钥，不能使用加密私钥。完整证书链仍要在启动后从外部网络用 `openssl s_client -verify_return_error` 验证。

然后修改 `.env`：

```dotenv
BRCLIO_AUTO_TLS=false
BRCLIO_ACME_EMAIL=
BRCLIO_TLS_CERT=/run/tls/fullchain.pem
BRCLIO_TLS_KEY=/run/tls/privkey.pem
```

自动 ACME 与静态证书不能同时启用。证书必须覆盖 `BRCLIO_HOSTNAME`，并用于 Web、465、587 STARTTLS 与 993。

#### 静态证书续期

应用只在进程启动时读取静态证书，替换文件后不会热加载。续期时保留旧文件，使用同目录的版本化新文件，并重新执行上面的权限、非加密私钥、有效期、主机名和 SPKI 匹配检查：

```bash
set -Eeuo pipefail
tls_stamp="$(date -u +%Y%m%dT%H%M%SZ)"
new_cert="secrets/tls/fullchain-${tls_stamp}.pem"
new_key="secrets/tls/privkey-${tls_stamp}.pem"
sudo install -o 10001 -g 10001 -m 0600 \
  /path/to/renewed-fullchain.pem "$new_cert"
sudo install -o 10001 -g 10001 -m 0600 \
  /path/to/renewed-privkey.pem "$new_key"

for file in "$new_cert" "$new_key"; do
  sudo test -f "$file"
  if sudo test -L "$file"; then
    printf 'TLS file must not be a symbolic link: %s\n' "$file" >&2
    exit 1
  fi
  test "$(sudo stat -c '%u:%g %a' "$file")" = '10001:10001 600'
done
sudo openssl x509 -in "$new_cert" -noout \
  -dates -checkhost mail.example.com
sudo openssl x509 -in "$new_cert" -checkend 604800 -noout
if sudo grep -Eq 'BEGIN ENCRYPTED PRIVATE KEY|Proc-Type: 4,ENCRYPTED' \
  "$new_key"; then
  echo 'encrypted private keys are not supported' >&2
  exit 1
fi
sudo openssl pkey -in "$new_key" -passin pass: -check -noout
new_cert_spki="$(sudo openssl x509 -in "$new_cert" -pubkey -noout | \
  openssl pkey -pubin -outform DER | sha256sum | cut -d' ' -f1)"
new_key_spki="$(sudo openssl pkey -in "$new_key" -passin pass: \
  -pubout -outform DER | sha256sum | cut -d' ' -f1)"
test "$new_cert_spki" = "$new_key_spki"

mkdir -p backups
chmod 0700 backups
env_backup="backups/tls-env-${tls_stamp}.env"
[[ ! -e "$env_backup" && ! -L "$env_backup" ]]
install -m 0600 -- .env "$env_backup"
cp -- .env .env.next
chmod 0600 .env.next
sed -i "s|^BRCLIO_TLS_CERT=.*|BRCLIO_TLS_CERT=/run/tls/${new_cert##*/}|" .env.next
sed -i "s|^BRCLIO_TLS_KEY=.*|BRCLIO_TLS_KEY=/run/tls/${new_key##*/}|" .env.next
grep -Fx "BRCLIO_TLS_CERT=/run/tls/${new_cert##*/}" .env.next
grep -Fx "BRCLIO_TLS_KEY=/run/tls/${new_key##*/}" .env.next
mv -f -- .env.next .env

if ! sudo docker compose -f docker-compose.yml up --pull never -d --no-build \
  --force-recreate --wait --wait-timeout 120 brclio-mail; then
  install -m 0600 -- "$env_backup" .env
  sudo docker compose -f docker-compose.yml up --pull never -d --no-build \
    --force-recreate --wait --wait-timeout 120 brclio-mail
  echo 'new certificate failed startup; old env was restored' >&2
  exit 1
fi
```

保持 SSH 会话与旧证书文件，立即从外部网络重复第 7 节的 443、465、587 STARTTLS、993 四项 `openssl s_client -verify_return_error`。任一失败就把 `$env_backup` 恢复为 `.env`、再次 `--force-recreate --wait`，并调查完整链与主机名；全部通过后才按保留策略清理旧证书。

### 5.3 外发前必须配置 smarthost

`.env`：

```dotenv
BRCLIO_RELAY_ADDR=smtp.provider.example:465
BRCLIO_RELAY_USERNAME=account@example.com
BRCLIO_RELAY_IMPLICIT_TLS=true
BRCLIO_DIRECT_DELIVERY=false
```

真实密码只交互写入主机 secret，不进入 shell history：

```bash
read -r -s -p 'Relay password: ' relay_password
printf '\n'
printf '%s' "$relay_password" | sudo tee secrets/relay_password >/dev/null
unset relay_password
sudo chown 10001:10001 secrets/relay_password
sudo chmod 0600 secrets/relay_password
```

不要把密码放进 `.env`、命令参数、截图或工单。提供商使用 `587 + STARTTLS` 时改为 `:587` 和 `false`。本教程保持 `BRCLIO_DIRECT_DELIVERY=false`；不配置 relay 时 doctor 虽能完成数据库检查，但会报告 `"deliveryMode":"disabled"`，外域邮件不会真正发出，因此下面的启动门禁会拒绝继续。

## 6. 发布前检查 Compose 模型

首次安装必须从不存在的固定名称 volume 开始；如果 `brclio-mail-data` 已存在，无论是否看似为空，都停止并确认它属于升级、恢复还是另一套 checkout，不能静默复用。随后在第一次 doctor 写入 SQLite 前确认 Docker 根目录、volume driver options 与实际落盘文件系统都不是网络或 FUSE 文件系统：

```bash
sudo bash <<'BRCLIO_VOLUME_CHECK'
set -Eeuo pipefail

if docker volume inspect brclio-mail-data >/dev/null 2>&1; then
  docker volume inspect brclio-mail-data
  docker ps -a --filter volume=brclio-mail-data
  echo 'existing brclio-mail-data volume requires explicit recovery/upgrade review' >&2
  exit 1
fi

docker_root="$(docker info --format '{{.DockerRootDir}}')"
docker_root_fstype="$(findmnt --target "$docker_root" -n -o FSTYPE)"
case "$docker_root_fstype" in
  ext2 | ext3 | ext4 | xfs | btrfs | zfs | f2fs | bcachefs) ;;
  *)
    printf 'Docker root is not on the reviewed local-filesystem allowlist: %s\n' \
      "$docker_root_fstype" >&2
    exit 1 ;;
esac

docker volume create brclio-mail-data >/dev/null
volume_driver="$(docker volume inspect -f '{{.Driver}}' brclio-mail-data)"
volume_options="$(docker volume inspect -f '{{json .Options}}' brclio-mail-data)"
volume_mountpoint="$(docker volume inspect -f '{{.Mountpoint}}' brclio-mail-data)"

[[ "$volume_driver" == "local" ]] || {
  printf 'unsupported volume driver: %s\n' "$volume_driver" >&2
  exit 1
}
case "$volume_options" in
  null | '{}') ;;
  *) printf 'volume options must be empty: %s\n' "$volume_options" >&2; exit 1 ;;
esac

volume_fstype="$(findmnt --target "$volume_mountpoint" -n -o FSTYPE)"
case "$volume_fstype" in
  ext2 | ext3 | ext4 | xfs | btrfs | zfs | f2fs | bcachefs) ;;
  *)
    printf 'volume is not on the reviewed local-filesystem allowlist: %s\n' \
      "$volume_fstype" >&2
    exit 1 ;;
esac
printf 'volume check passed: driver=%s docker-root=%s volume=%s\n' \
  "$volume_driver" "$docker_root_fstype" "$volume_fstype"
BRCLIO_VOLUME_CHECK
```

只有上面的门禁成功后，才构建并运行预启动 doctor：

```bash
set -Eeuo pipefail
target_version="$(git describe --tags --exact-match)"
target_commit="$(git rev-parse HEAD)"
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
expected_version="${target_version#v}"
expected_image="brclio-mail:${expected_version}"
sudo docker compose -f docker-compose.yml config --quiet
sudo docker compose -f docker-compose.yml config --images | \
  grep -Fx "$expected_image"
sudo env \
  BRCLIO_COMMIT="$target_commit" \
  BRCLIO_BUILD_DATE="$build_date" \
  docker compose -f docker-compose.yml build --pull brclio-mail
version_output="$(sudo docker compose -f docker-compose.yml run \
  --pull never --rm --no-deps brclio-mail version)"
printf '%s\n' "$version_output"
grep -F "brclio-mail ${expected_version} (commit ${target_commit}," \
  <<<"$version_output"
doctor_output="$(sudo docker compose -f docker-compose.yml run \
  --pull never --rm --no-deps brclio-mail doctor)"
printf '%s\n' "$doctor_output"
grep -F '"deliveryMode":"smarthost"' <<<"$doctor_output"
```

一次性 `run` 不带 `--service-ports`，因此不会开放公网端口；`version` 核对构建身份，`doctor` 使用 named volume 初始化/检查 SQLite 并报告 TLS/投递模式，教程还硬性断言 smarthost 已启用。在 doctor 成功前不要执行 `up`。

当前 doctor **不会**验证 `BRCLIO_BASE_URL` 的生产 URL 规则、实际解析静态证书链、绑定六个监听器或登录 smarthost；因此静态 TLS 必须完成第 5.2 节检查，端口必须保持空闲，启动后还要做外部 TLS 与真实收发验收。doctor 成功不能替代这些步骤。

## 7. 启动服务

```bash
if ! sudo docker compose -f docker-compose.yml up --pull never -d --no-build \
  --wait --wait-timeout 120 brclio-mail; then
  sudo docker compose -f docker-compose.yml logs --tail 100 brclio-mail
  sudo docker compose -f docker-compose.yml stop brclio-mail
  echo 'container did not become healthy; service was stopped' >&2
  exit 1
fi
sudo docker compose -f docker-compose.yml ps
sudo docker compose -f docker-compose.yml logs --tail 100 brclio-mail
```

检查：

```bash
curl -fsS https://mail.example.com/healthz
sudo docker compose -f docker-compose.yml exec -T \
  brclio-mail brclio-mail version
running_doctor="$(sudo docker compose -f docker-compose.yml exec -T \
  brclio-mail brclio-mail doctor)"
printf '%s\n' "$running_doctor"
grep -F '"deliveryMode":"smarthost"' <<<"$running_doctor"
```

Compose 的容器 healthcheck 只探测容器内 `8443` 是否有 TCP 监听，不能代替 HTTPS 证书、SMTP/IMAP 和真实收发验收。

从服务器之外的网络验证所有入口和证书链：

```bash
for port in 25 80 443 465 587 993; do
  nc -vz mail.example.com "$port"
done
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
```

## 8. 首次管理员初始化

```bash
sudo cat secrets/setup_token
```

打开 `https://mail.example.com` 创建首位管理员。完成后轮换 token 并重建容器，使 secret 重新挂载：

```bash
sudo sh -c 'umask 077; openssl rand -base64 36 > secrets/setup_token'
sudo chown 10001:10001 secrets/setup_token
sudo chmod 0600 secrets/setup_token
sudo docker compose -f docker-compose.yml up --pull never -d --no-build \
  --force-recreate --wait --wait-timeout 120 brclio-mail
```

## 9. 域名、DNS 与客户端

首次管理员初始化会同时创建表单中填写的首个 `pending` 邮件域。建议先完成主机 A、PTR、TLS、首次管理员与外部端口验收，再查看该域并发布管理台生成的验证记录；只有新增其他域时才再使用“添加域名”：

```text
_brclio-mail.example.com. TXT "<管理台生成的 token>"
```

等待后台状态变为 `verified`，再发布管理台生成的 DKIM、按真实出口编写唯一 SPF，并以 `p=none` 开始 DMARC；最后才发布 MX，避免未验收服务过早接收真实邮件。TLS-RPT 与 SRV 按需添加。完整说明见 [DNS 文档](dns.md)。MX 目标不能是裸 IP 或 CNAME。

第三方客户端：

| 用途 | 主机 | 端口 | 加密 |
| --- | --- | ---: | --- |
| IMAP 收信 | `mail.example.com` | `993` | SSL/TLS |
| SMTP 发信 | `mail.example.com` | `465` | SSL/TLS |
| SMTP 备选 | `mail.example.com` | `587` | STARTTLS |

每台设备使用独立应用密码，见[第三方邮件客户端](clients.md)。

## 10. Docker 防火墙边界

Docker 发布端口会创建自己的 NAT/防火墙规则。Docker 官方明确提醒：发布的容器端口可能绕过部分 UFW/firewalld 规则。因此不能只看到 UFW “deny” 就认为容器端口不可达。

- 同时配置云厂商安全组；
- 理解 Docker 的 [端口发布](https://docs.docker.com/engine/network/port-publishing/)；
- 需要额外来源限制时使用 Docker 支持的主机防火墙链，例如 `DOCKER-USER`；
- 从服务器之外实际探测六个端口；
- Docker socket 和 `docker` 用户组等同高权限，限制能访问它们的账号。

邮件服务的 25/80/443/465/587/993 通常需要公开，而 SSH、Docker daemon API 和管理面板端口不应对所有来源开放。

## 11. SQLite volume 与一致性备份

确认 named volume 使用本机 `local` driver：

```bash
sudo docker volume inspect brclio-mail-data
```

![Bornforthis 守住本机 SQLite 卷并剪断跨主机共享线](../assets/docker-deployment-illustrations/02-keep-sqlite-volume-local.png)

不要把 `brclio-mail-data` 换成 NFS、SMB、CephFS、GlusterFS 或对象存储 FUSE，也不要让两个容器、systemd 服务和容器同时访问它。

Compose project 与 volume 名固定为 `brclio-mail`，同机启动第二套 checkout 会冲突。`.env` 和 `secrets/` 虽被 Git 忽略，但 `git clean -fdx` 会删除它们；必须单独加密备份。不要执行 `docker volume prune` 清理尚未确认归属的 volume。

创建一致性备份并复制到宿主机：

```bash
set -Eeuo pipefail

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_output="backups/brclio-mail-${stamp}.sqlite"
backup_staging=""
cleanup_backup_staging() {
  if [[ -n "$backup_staging" && \
        "$backup_staging" == /var/tmp/brclio-mail-export.* ]]; then
    sudo rm -rf -- "$backup_staging"
  fi
}
trap cleanup_backup_staging EXIT

sudo docker compose -f docker-compose.yml exec -T brclio-mail \
  brclio-mail backup "/data/backups/brclio-mail-${stamp}.sqlite"

mkdir -p backups
chmod 0700 backups
[[ ! -e "$backup_output" && ! -L "$backup_output" ]] || {
  printf 'refusing to overwrite backup output: %s\n' "$backup_output" >&2
  exit 1
}

backup_staging="$(sudo mktemp -d /var/tmp/brclio-mail-export.XXXXXX)"
[[ "$backup_staging" == /var/tmp/brclio-mail-export.* ]]
sudo docker compose -f docker-compose.yml cp \
  "brclio-mail:/data/backups/brclio-mail-${stamp}.sqlite" \
  "${backup_staging}/snapshot.sqlite"
sudo test -f "${backup_staging}/snapshot.sqlite"
if sudo test -L "${backup_staging}/snapshot.sqlite"; then
  echo 'refusing symbolic-link backup export' >&2
  exit 1
fi
sudo install -o "$(id -u)" -g "$(id -g)" -m 0600 -- \
  "${backup_staging}/snapshot.sqlite" "$backup_output"

integrity_result="$(sqlite3 "$backup_output" \
  'PRAGMA integrity_check;')"
test "$integrity_result" = "ok" || {
  printf 'integrity_check failed: %s\n' "$integrity_result" >&2
  exit 1
}
foreign_key_violations="$(sqlite3 "$backup_output" \
  'PRAGMA foreign_key_check;')"
test -z "$foreign_key_violations" || {
  printf 'foreign_key_check failed:\n%s\n' "$foreign_key_violations" >&2
  exit 1
}
sha256sum "$backup_output"
cleanup_backup_staging
backup_staging=""
trap - EXIT
```

两项检查都无报错并继续打印 SHA-256 才算通过。备份包含邮件、附件、密码哈希、DKIM 私钥和管理员归档，必须加密并移出主机。

`backup` 会先在 named volume 的 `/data/backups/` 生成一份完整 SQLite，上面的导出又在宿主机保存一份。如果不做保留策略，它们会绕开邮箱归档上限并最终吃满磁盘。应当监控宿主和 volume 剩余空间，明确保留份数/天数；只有在异地加密副本的 hash 也已核验后，才删除精确的容器内副本：

```bash
set -Eeuo pipefail
snapshot_basename="brclio-mail-20260825T120000Z.sqlite" # 也可为已验证的 pre-upgrade-...sqlite
[[ "$snapshot_basename" =~ ^(brclio-mail|pre-upgrade)-[0-9]{8}T[0-9]{6}Z\.sqlite$ ]]
sudo docker compose -f docker-compose.yml exec -T \
  -e SNAPSHOT_BASENAME="$snapshot_basename" \
  brclio-mail /bin/sh -ceu '
    case "$SNAPSHOT_BASENAME" in
      brclio-mail-????????T??????Z.sqlite | \
      pre-upgrade-????????T??????Z.sqlite) ;;
      *) echo "unsafe snapshot name" >&2; exit 1 ;;
    esac
    snapshot="/data/backups/${SNAPSHOT_BASENAME}"
    test -f "$snapshot"
    test ! -L "$snapshot"
    rm -- "$snapshot"
  '
```

宿主机 `backups/` 也不是异地备份；按同一保留策略清理，并至少保留一份在另一台机器或对象存储中。

## 12. 安全升级

升级前先按第 11 节做一份日常灾备并移出主机；它不是下面的回滚快照。回滚快照必须在旧服务停止后才创建，否则旧服务在构建新镜像期间收到的邮件会消失于回滚点之后。使用静态证书时，还要重新执行第 5.2 节检查。

下面是一个完整、失败即停止的升级流程。只修改第一行的目标 Release tag，然后在仓库根目录一次性执行：

```bash
target_version="vX.Y.Z" # 必须替换为已审阅的新 Release tag
set -Eeuo pipefail

[[ "$target_version" =~ ^v[0-9][0-9A-Za-z._-]*$ && \
   "$target_version" != "vX.Y.Z" ]] || {
  echo 'replace target_version with a reviewed Release tag' >&2
  exit 1
}
repo_root="$(pwd -P)"
[[ -f "$repo_root/docker-compose.yml" && -f "$repo_root/.env" && \
   ! -L "$repo_root/docker-compose.yml" && ! -L "$repo_root/.env" ]]
test -z "$(git status --porcelain)"

backup_dir="$repo_root/backups"
if [[ -e "$backup_dir" || -L "$backup_dir" ]]; then
  [[ -d "$backup_dir" && ! -L "$backup_dir" ]]
else
  install -d -m 0700 -- "$backup_dir"
fi
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
rollback_git_commit="$(git rev-parse HEAD)"
rollback_env="$backup_dir/compose-rollback-${stamp}.env"
rollback_compose="$backup_dir/docker-compose-rollback-${stamp}.yml"
rollback_manifest="$backup_dir/upgrade-rollback-${stamp}.txt"
rollback_backup="$backup_dir/pre-upgrade-${stamp}.sqlite"
rollback_basename="${rollback_backup##*/}"
[[ ! -e "$rollback_env" && ! -L "$rollback_env" && \
   ! -e "$rollback_compose" && ! -L "$rollback_compose" && \
   ! -e "$rollback_manifest" && ! -L "$rollback_manifest" && \
   ! -e "$rollback_backup" && ! -L "$rollback_backup" ]]

old_version_output="$(sudo docker compose -f docker-compose.yml exec -T \
  brclio-mail brclio-mail version)"
printf '%s\n' "$old_version_output"
read -r old_product rollback_version _ <<<"$old_version_output"
[[ "$old_product" == 'brclio-mail' && -n "$rollback_version" ]]
old_container="$(sudo docker compose -f docker-compose.yml ps -q \
  brclio-mail)"
[[ -n "$old_container" && \
   "$(sudo docker inspect -f '{{.State.Running}}' "$old_container")" == true ]]
old_image_id="$(sudo docker inspect -f '{{.Image}}' "$old_container")"
[[ "$old_image_id" == sha256:* ]]
configured_image="$(sudo docker compose -f docker-compose.yml \
  config --images)"
[[ -n "$configured_image" && "$configured_image" != *$'\n'* ]]
configured_image_id="$(sudo docker image inspect -f '{{.Id}}' \
  "$configured_image")"
[[ "$configured_image_id" == "$old_image_id" ]] || {
  echo 'running container image differs from the image resolved by current .env' >&2
  exit 1
}
old_image_commit="$(sudo docker image inspect -f \
  '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
  "$old_image_id")"
old_image_version="$(sudo docker image inspect -f \
  '{{index .Config.Labels "org.opencontainers.image.version"}}' \
  "$old_image_id")"
[[ "$old_image_commit" == "$rollback_git_commit" && \
   "$old_image_version" == "$rollback_version" ]] || {
  echo 'running image OCI commit/version does not match current checkout' >&2
  exit 1
}
config_hash_line="$(sudo docker compose -f docker-compose.yml \
  config --hash brclio-mail)"
read -r config_hash_service current_config_hash config_hash_extra \
  <<<"$config_hash_line"
running_config_hash="$(sudo docker inspect -f \
  '{{index .Config.Labels "com.docker.compose.config-hash"}}' \
  "$old_container")"
[[ "$config_hash_service" == 'brclio-mail' && \
   "$current_config_hash" =~ ^[0-9a-f]{64}$ && \
   -z "${config_hash_extra:-}" && \
   "$running_config_hash" == "$current_config_hash" ]] || {
  echo 'running container Compose config differs from current .env/Compose' >&2
  exit 1
}
rollback_image="brclio-mail:rollback-${stamp}"
sudo docker image tag "$old_image_id" "$rollback_image"

install -m 0600 -- .env "$rollback_env"
install -m 0600 -- docker-compose.yml "$rollback_compose"
grep -q '^BRCLIO_IMAGE=' "$rollback_env"
grep -q '^BRCLIO_VERSION=' "$rollback_env"
sed -i "s|^BRCLIO_IMAGE=.*|BRCLIO_IMAGE=${rollback_image}|" \
  "$rollback_env"
sed -i "s|^BRCLIO_VERSION=.*|BRCLIO_VERSION=${rollback_version}|" \
  "$rollback_env"
grep -Fx "BRCLIO_IMAGE=${rollback_image}" "$rollback_env"
grep -Fx "BRCLIO_VERSION=${rollback_version}" "$rollback_env"
sudo docker compose --project-directory "$repo_root" \
  --project-name brclio-mail --env-file "$rollback_env" \
  -f "$rollback_compose" config --images | grep -Fx "$rollback_image"

{
  printf 'rollback_git_commit=%s\n' "$rollback_git_commit"
  printf 'rollback_image=%s\n' "$rollback_image"
  printf 'rollback_image_id=%s\n' "$old_image_id"
  printf 'rollback_version=%s\n' "$rollback_version"
  printf 'rollback_env=%s\n' "$rollback_env"
  printf 'rollback_compose=%s\n' "$rollback_compose"
} >"$rollback_manifest"
chmod 0600 "$rollback_manifest"

# 旧容器仍在服务；先构建并核验新镜像。
git fetch --tags
git checkout "$target_version"
test -z "$(git status --porcelain)"
test "$(git describe --tags --exact-match)" = "$target_version"
target_commit="$(git rev-parse HEAD)"
expected_version="${target_version#v}"
expected_image="brclio-mail:${expected_version}"
grep -q '^BRCLIO_IMAGE=' .env
grep -q '^BRCLIO_VERSION=' .env
sed -i "s|^BRCLIO_IMAGE=.*|BRCLIO_IMAGE=${expected_image}|" .env
sed -i "s|^BRCLIO_VERSION=.*|BRCLIO_VERSION=${expected_version}|" .env
grep -Fx "BRCLIO_IMAGE=${expected_image}" .env
grep -Fx "BRCLIO_VERSION=${expected_version}" .env
sudo docker compose -f docker-compose.yml config --images | \
  grep -Fx "$expected_image"
sudo env \
  BRCLIO_COMMIT="$target_commit" \
  BRCLIO_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  docker compose -f docker-compose.yml build --pull brclio-mail
version_output="$(sudo docker compose -f docker-compose.yml run \
  --pull never --rm --no-deps brclio-mail version)"
printf '%s\n' "$version_output"
grep -F "brclio-mail ${expected_version} (commit ${target_commit}," \
  <<<"$version_output"
printf 'target_version=%s\ntarget_commit=%s\n' \
  "$target_version" "$target_commit" >>"$rollback_manifest"

# 从这里开始进入停机维护窗口。在新 doctor 之前任一步失败，重启精确的旧容器。
restart_old_enabled=true
snapshot_staging=""
cleanup_snapshot_staging() {
  if [[ -n "${snapshot_staging:-}" && \
        "$snapshot_staging" == /var/tmp/brclio-mail-upgrade.* ]]; then
    if ! sudo rm -rf -- "$snapshot_staging"; then
      printf 'warning: could not remove staging directory: %s\n' \
        "$snapshot_staging" >&2
    fi
  fi
}
restart_old_before_doctor() {
  exit_status=$?
  cleanup_snapshot_staging
  if [[ "$exit_status" -ne 0 && "$restart_old_enabled" == true ]]; then
    old_container_running=""
    if ! old_container_running="$(sudo docker inspect -f \
      '{{.State.Running}}' "$old_container")"; then
      echo 'CRITICAL: cannot inspect old container state' >&2
    elif [[ "$old_container_running" == true ]]; then
      echo 'pre-doctor upgrade failed; the unchanged old container remains running' >&2
    elif [[ "$old_container_running" != false ]]; then
      printf 'CRITICAL: unexpected old container state: %s\n' \
        "$old_container_running" >&2
    else
      restart_volume_users=""
      if ! restart_volume_users="$(sudo docker ps -q \
        --filter volume=brclio-mail-data)"; then
        echo 'CRITICAL: cannot inspect volume users; old container was not restarted' >&2
      elif [[ -n "$restart_volume_users" ]]; then
        printf 'CRITICAL: volume still used by %s; old container was not restarted\n' \
          "$restart_volume_users" >&2
      elif sudo docker start "$old_container" >/dev/null; then
        echo 'pre-doctor upgrade failed; the unchanged old container was restarted' >&2
      else
        echo 'CRITICAL: pre-doctor upgrade failed and old container restart also failed' >&2
      fi
    fi
  fi
  exit "$exit_status"
}
trap restart_old_before_doctor EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sudo docker stop -t 60 "$old_container"
[[ "$(sudo docker inspect -f '{{.State.Running}}' "$old_container")" == false ]]
if ! volume_users="$(sudo docker ps -q \
  --filter volume=brclio-mail-data)"; then
  echo 'cannot inspect containers using brclio-mail-data' >&2
  exit 1
fi
[[ -z "$volume_users" ]] || {
  printf 'volume is still used by: %s\n' "$volume_users" >&2
  exit 1
}

# 用已保留的旧镜像+旧配置，在停机后创建最终一致快照。
sudo docker compose --project-directory "$repo_root" \
  --project-name brclio-mail --env-file "$rollback_env" \
  -f "$rollback_compose" run --pull never --rm --no-deps \
  brclio-mail backup "/data/backups/${rollback_basename}"
if ! volume_users="$(sudo docker ps -q \
  --filter volume=brclio-mail-data)"; then
  echo 'cannot re-check containers using brclio-mail-data' >&2
  exit 1
fi
[[ -z "$volume_users" ]] || {
  printf 'snapshot container is still using the volume: %s\n' \
    "$volume_users" >&2
  exit 1
}

snapshot_staging="$(sudo mktemp -d /var/tmp/brclio-mail-upgrade.XXXXXX)"
sudo docker cp \
  "${old_container}:/data/backups/${rollback_basename}" \
  "${snapshot_staging}/snapshot.sqlite"
sudo test -f "${snapshot_staging}/snapshot.sqlite"
if sudo test -L "${snapshot_staging}/snapshot.sqlite"; then
  echo 'refusing symbolic-link rollback snapshot' >&2
  exit 1
fi
sudo install -o "$(id -u)" -g "$(id -g)" -m 0600 -- \
  "${snapshot_staging}/snapshot.sqlite" "$rollback_backup"
[[ -f "$rollback_backup" && ! -L "$rollback_backup" ]]
if ! rollback_integrity="$(sqlite3 -batch -bail "$rollback_backup" \
  'PRAGMA integrity_check;')"; then
  echo 'cannot verify stopped-state rollback snapshot' >&2
  exit 1
fi
[[ "$rollback_integrity" == 'ok' ]]
if ! rollback_fk="$(sqlite3 -batch -bail "$rollback_backup" \
  'PRAGMA foreign_key_check;')"; then
  echo 'cannot check rollback snapshot foreign keys' >&2
  exit 1
fi
[[ -z "$rollback_fk" ]]
rollback_sha256="$(sha256sum "$rollback_backup" | awk '{print $1}')"
[[ "$rollback_sha256" =~ ^[0-9a-f]{64}$ ]]
printf 'rollback_backup=%s\nrollback_sha256=%s\n' \
  "$rollback_backup" "$rollback_sha256" >>"$rollback_manifest"
cleanup_snapshot_staging
snapshot_staging=""
printf 'rollback mapping saved: %s\n' "$rollback_manifest"

# 只有最终快照导出、SQLite/FK 和 hash 全部通过后才运行新 doctor。
# doctor 可能执行新版本数据库迁移；从此刻起禁止直接重启旧容器，改用成对回滚流程。
restart_old_enabled=false
trap - HUP INT TERM EXIT
if ! doctor_output="$(sudo docker compose -f docker-compose.yml run \
  --pull never --rm --no-deps brclio-mail doctor)"; then
  echo 'new image doctor failed; service remains stopped' >&2
  exit 1
fi
printf '%s\n' "$doctor_output"
if ! grep -F '"deliveryMode":"smarthost"' <<<"$doctor_output"; then
  echo 'smarthost is not active; service remains stopped' >&2
  exit 1
fi
if ! sudo docker compose -f docker-compose.yml up --pull never -d \
  --no-build --force-recreate --wait --wait-timeout 120 brclio-mail; then
  sudo docker compose -f docker-compose.yml logs --tail 100 brclio-mail
  sudo docker compose -f docker-compose.yml stop -t 60 brclio-mail
  echo 'new container failed readiness and was stopped' >&2
  exit 1
fi
if ! curl -fsS https://mail.example.com/healthz; then
  sudo docker compose -f docker-compose.yml stop -t 60 brclio-mail
  echo 'post-start health check failed; service was stopped for investigation' >&2
  exit 1
fi
```

`--project-directory` 保证保存到 `backups/` 的旧 Compose 文件仍以仓库根目录解析 `secrets/` 等相对路径；`run --pull never` 不发布服务端口，也不会悄悄拉取另一个镜像。升级前的 OCI commit/version、本地 image ID 和 Compose 配置 hash 比对，会拒绝“仓库/配置已改，容器却还是旧的”混合状态。`config --hash` 的定义见 [Docker Compose 官方文档](https://docs.docker.com/reference/cli/docker/compose/config/)。

在新 doctor 开始前，快照/导出/校验失败会自动重启未更改的旧容器；doctor 可能执行数据库迁移，因此开始 doctor 后不再自动重启旧镜像。Docker 路线目前没有 systemd 升级脚本那样的自动成对回滚。若新镜像 doctor 失败，服务保持停止；若启动后健康检查失败，上面会立即停止它。使用映射文件并按[Docker 升级失败回滚](operations.md#docker-升级失败回滚)成对恢复旧 commit、旧 Compose、旧 `.env`、旧镜像与这份**停机后**快照。

回滚快照的 volume 内副本是一个例外：先保留它。当新版本已完成 HTTPS/SMTP/IMAP/真实收发和恢复演练，且宿主与异地副本均验证 hash 后，才按第 11 节的精确删除方式清理它。宿主的 `rollback_backup`、映射文件和旧镜像保留多久，应写入公司的保留策略。新服务已经可能接收邮件后不能盲目恢复旧快照，否则会丢失升级后的新邮件。

## 13. 恢复顺序

完整替换 SQLite 的安全命令见[运维文档的 Docker Compose 运维对照](operations.md#docker-compose-运维对照)。恢复时必须遵守：

1. 停止服务并用 `docker ps --filter volume=brclio-mail-data` 确认没有其他容器使用目标 volume；
2. 验证恢复文件完整性；
3. 在 root 一次性容器中保留旧 DB/WAL/SHM，再安装快照；
4. 保持公网监听器关闭，先运行：

```bash
sudo docker compose -f docker-compose.yml run --pull never --rm --no-deps \
  brclio-mail doctor
```

5. doctor 成功后才执行：

```bash
sudo docker compose -f docker-compose.yml up --pull never -d --no-build \
  --wait --wait-timeout 120 brclio-mail
```

不能先 `up -d` 再 doctor，否则待恢复数据库会在检查前暴露给 Web、SMTP 和 IMAP。

## 14. 停止与卸载

停止并删除容器、保留数据 volume：

```bash
sudo docker compose -f docker-compose.yml down
sudo docker volume inspect brclio-mail-data
```

不要随意使用 `down -v`，它会删除 named volume。只有在已经完成异地备份、恢复验证，并明确决定永久销毁全部邮件数据时，才单独制定删除步骤。

## 15. 最终验收

- `docker compose ps` 显示容器运行且 health 正常；
- named volume 为本机 local driver，只有一个服务实例使用；
- 从外部网络验证 HTTPS、25、465、587、993；
- 管理台域名为 `verified`；
- 完成真实外部收发与 IMAP 同步；
- SPF/DKIM/DMARC 与实际出口对齐；
- 创建、复制、校验并异地保存一致性备份；
- 披露管理员可以读取全部往来邮件及用户视图中已删除邮件；
- 记录 Docker Engine、Compose、源码 tag 和镜像构建时间，升级时可追溯。

仓库 CI 当前验证 Compose 模型与 amd64 镜像构建，但不等于已经在真实 Docker 主机完成 ACME、六端口、SMTP/IMAP、arm64 容器和收发冒烟测试；这些必须由部署者按本节验收。

继续阅读：[宝塔快速部署](tutorial-baota-quick.md) · [宝塔完整版](tutorial-baota.md) · [命令行与一键部署](tutorial-command-line.md) · [运维、备份与恢复](operations.md)
