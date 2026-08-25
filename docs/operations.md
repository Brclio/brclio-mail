# 运维、备份与恢复

本文以首选的 Linux systemd 部署为主，假设使用安装包默认的 `/etc/brclio-mail`、`/var/lib/brclio-mail`、`/var/backups/brclio-mail` 和 `/usr/local/bin/brclio-mail`。事务式升级脚本会主动拒绝非标准 data/database 路径；自定义布局不能只对本文命令做搜索替换，必须单独设计、备份并演练迁移/升级。Docker 用户见 [Docker Compose 运维对照](#docker-compose-运维对照)。

## 日常健康检查

至少同时检查外部 HTTPS、systemd 状态、数据库自检和日志：

```bash
curl -fsS https://mail.example.com/healthz
sudo systemctl is-active brclio-mail
sudo systemctl status brclio-mail --no-pager
sudo journalctl -u brclio-mail --since "30 minutes ago" --no-pager
```

`/healthz` 会检查数据库可达性，但不是证书、SMTP/IMAP 或端到端投递检查。`doctor` 会执行完整的 `PRAGMA integrity_check` 与 `PRAGMA foreign_key_check`；大数据库可能耗时，不应作为每次 systemd 启动的短超时前置条件。维护窗口中使用随安装包提供的受限 oneshot unit：

```bash
sudo systemctl start brclio-mail-doctor.service
sudo journalctl -u brclio-mail-doctor.service -n 100 --no-pager
```

`systemctl start` 会等待检查完成并在失败时返回非零；安装包提供的 unit 给完整检查最多 2 小时，成功的 oneshot 随后显示为 `inactive (dead)` 是正常状态。journal 应报告 `status: ok`、SQLite 版本、数据库路径、初始化状态、用户数、TLS 模式和投递模式。失败或超时需要调查，不能通过删除 WAL、跳过检查或改数据库来“修复”。

主应用和队列通过 stdout/stderr 输出结构化日志，systemd 部署由 journald 收集。按组织要求设置 journal 容量/保留期、集中转发和读取权限；不要把邮件正文、secret、完整认证输入或未脱敏远端响应复制到工单。

还应监控：

- `/var/lib/brclio-mail` 所在文件系统的剩余空间、inode 与 I/O 错误；默认低于 1 GiB 时拒绝新邮件；
- `/var/backups/brclio-mail` 所在文件系统的剩余空间和 root 管理的保留策略；
- 管理员队列中持续重试/失败的收件人域；
- 管理员归档保守物理估算与默认 100 GiB cap；
- Web、SMTP 与 IMAP 证书到期时间；
- 外部网络对 `25/443/465/587/993` 的可达性；
- 真实外部收发与 `dmarc@`、`tlsrpt@` 报告邮箱。

当前没有 Prometheus/OpenTelemetry 指标、内置告警或 DMARC/TLS-RPT 报表解析。

## 在线备份

不要用 `cp` 复制运行中的 `.db`、`-wal`、`-shm`。内置 `backup` CLI 创建 SQLite 一致性快照，并在交付前执行完整性与外键校验；目标文件不能已存在。原理见 [SQLite 在线备份](https://www.sqlite.org/backup.html)。

```bash
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_name="manual-${stamp}"
backup_path="/var/backups/brclio-mail/${backup_name}.sqlite"
df -h /var/lib/brclio-mail /var/backups/brclio-mail
if ! sudo systemctl start "brclio-mail-backup@${backup_name}.service"; then
  sudo journalctl -u "brclio-mail-backup@${backup_name}.service" -n 100 --no-pager
  echo "backup failed; no published snapshot is available" >&2
  exit 1
fi
sudo journalctl -u "brclio-mail-backup@${backup_name}.service" -n 50 --no-pager
sudo stat -c '%U:%G %a %s %n' "$backup_path"

mkdir -p backups
chmod 700 backups
sudo install -o "$(id -un)" -g "$(id -gn)" -m 0600 \
  "$backup_path" "backups/brclio-mail-${stamp}.sqlite"
```

在有 `sqlite3` 的隔离主机复核：

```bash
sqlite3 "backups/brclio-mail-${stamp}.sqlite" \
  'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

预期第一行是 `ok`，且 `foreign_key_check` 没有输出。安装包提供的 backup unit 在 root 管理的 `.staging`/`.incoming` 区生成并验证快照，最后由 root 以 `root:root`、`0600` 发布到 `/var/backups/brclio-mail`；主邮件服务不能写这些备份路径。unit 会预检数据库、WAL、目标文件系统和额外 1 GiB 余量；应用备份默认最多 2 小时，unit 允许 2 小时 15 分钟。失败或超时不会发布最终路径，应先看 journal 和 `/var/backups/brclio-mail/.staging`、`.incoming` 的占用，不要把未发布文件当成有效备份。确认离机加密副本可读后，按书面保留策略处理本机快照；长期保留在同一磁盘会消耗恢复余量，也不能防主机或磁盘整体故障。

快照包含原始 MIME、附件、用户与别名、密码哈希、会话、队列、审计、管理员归档和 DKIM 私钥。它不包含 `/etc/brclio-mail` 中的环境配置、setup/relay secret 或静态 TLS 私钥；这些必须单独加密备份。自动 ACME cache 位于 `/var/lib/brclio-mail/acme`，通常可重新申请，但受 CA 速率限制。

至少保留一个不同故障域的离线/异地加密副本，限制取用人员并定期在隔离主机完成真实恢复。仅看到“备份文件存在”不算恢复验证。

## systemd 恢复演练

恢复会中断 Web、SMTP 和 IMAP。先确认备份文件、默认数据库路径与服务名，绝不能把命令指向不相关目录。

1. 为当前实例再做一份在线备份并复制出主机。
2. 用上面的 `sqlite3` 命令验证待恢复文件。
3. 停止服务并确认已经停止：

```bash
sudo systemctl stop brclio-mail
if [ "$(systemctl is-active brclio-mail)" != "inactive" ]; then
  echo "service did not stop" >&2
  exit 1
fi
```

4. 先验证数据目录本身不是符号链接，再把当前 DB/WAL/SHM 的**路径条目**移到 root-only、随机命名的安全目录，然后安装已验证的快照。不能在服务账号可写的 `/var/lib/brclio-mail` 下由 root 创建可预测安全目录；还必须处理 broken symlink。示例文件名必须替换成实际简单文件名：

```bash
set -euo pipefail
restore_file="brclio-mail-20260825T120000Z.sqlite"
restore_source="${PWD}/backups/${restore_file}"
if [ ! -f "$restore_source" ] || [ -L "$restore_source" ]; then
  echo "restore source is missing, not regular, or a symbolic link" >&2
  exit 1
fi
if ! sudo test -d /var/lib/brclio-mail || sudo test -L /var/lib/brclio-mail; then
  echo "refusing missing or symbolic-link data directory" >&2
  exit 1
fi

safety_dir="$(sudo mktemp -d /var/backups/brclio-mail/restore-safety.XXXXXXXXXX)"
sudo chown root:root "$safety_dir"
sudo chmod 0700 "$safety_dir"
printf 'Safety directory: %s\n' "$safety_dir"
for file in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
  live_path="/var/lib/brclio-mail/${file}"
  if sudo test -e "$live_path" || sudo test -L "$live_path"; then
    sudo mv -- "$live_path" "$safety_dir/$file"
  fi
done
for file in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
  live_path="/var/lib/brclio-mail/${file}"
  if sudo test -e "$live_path" || sudo test -L "$live_path"; then
    echo "data path was recreated unexpectedly: $live_path" >&2
    exit 1
  fi
done
sudo install -o brclio-mail -g brclio-mail -m 0600 \
  "$restore_source" /var/lib/brclio-mail/brclio-mail.db
```

5. 在服务仍停止时运行安装包提供的 doctor unit。只有成功后才启动：

```bash
if ! sudo systemctl start brclio-mail-doctor.service; then
  sudo journalctl -u brclio-mail-doctor.service -n 100 --no-pager
  echo "restored database failed doctor; service remains stopped" >&2
  exit 1
fi
sudo journalctl -u brclio-mail-doctor.service -n 100 --no-pager
sudo systemctl start brclio-mail
sudo systemctl status brclio-mail --no-pager
curl -fsS https://mail.example.com/healthz
```

随后验证管理员/普通用户登录、随机邮件正文与附件、本地投递、外部收发、IMAP、队列、管理员归档和审计。任何一步失败都应停止服务、保留现场，并使用脚本打印的 `$safety_dir` 与开始前移出的在线备份回滚；确认长期恢复稳定且另有可读离机副本后，才按保留策略处理这个 root-only 安全目录。

## systemd 升级与回滚

不要无人值守地对邮件主机执行 `git pull`。先阅读目标版本变更，检出明确 tag/commit，完成一份已移出主机且验证可读的备份，然后：

```bash
target_version="v0.2.1-preview"
git fetch --tags
git checkout "$target_version"
sudo ./scripts/upgrade-systemd.sh --version "$target_version"
/usr/local/bin/brclio-mail version
```

若使用已经审阅的本地源码构建，先运行 `make build`，再改传 `--binary "$PWD/bin/brclio-mail"`。Release 路径会先在旧服务仍可用时下载 tar、核对 checksum 与 `brclio-mail <version>` 身份；只有预取成功后，升级脚本才停止 active 服务并确认 inactive，再用**当前旧二进制**创建一致性 pre-upgrade 数据库快照，并把旧二进制、三个 unit 与 backup helper 保存为同时间戳 package 快照。随后安装新 package，在服务仍停止时运行新版本 `doctor`，只有成功才重新启动原本 active 的服务并核验状态/本机 HTTPS。安装、预启动 doctor 或可捕获的 HUP/INT/TERM 中断会在监听器开放前自动成对恢复旧数据库和 package；监听器开放后的检查失败则不会自动恢复，以免丢弃可能已经接收的新邮件。断电与 SIGKILL 无法由 shell trap 处理，必须使用打印的快照人工恢复。升级因此有短暂停机；远端 MTA 通常会重试，但必须安排维护窗口并告知 Web/IMAP 用户。脚本打印的数据库快照、package 快照与 convenience `.previous` binary 路径必须记录并移出主机。

不能只把二进制换旧：数据库迁移可能不向后兼容。若监听器重新开放后才需要回滚，必须把**匹配的 pre-upgrade SQLite 快照和同时间戳 package 快照**作为一组恢复：

1. 立即停止新服务并保存当前 post-upgrade 数据库；如果升级后已接受新邮件，恢复旧快照会丢失这些新增状态，必须先评估和保全；
2. 仅按上一节第 4 步的安全路径替换流程安装匹配的 pre-upgrade 快照，此时跳过第 5 步，继续保持监听器关闭；
3. 从脚本打印的 `pre-upgrade-package-*` 目录恢复旧二进制、三个 unit 与 backup helper，再执行 `daemon-reload`；只有在 package 快照已经另行核验缺失时，才把 `.previous` binary 和同一旧 commit 的 unit/helper 作为人工恢复来源；
4. 用旧二进制运行 `doctor`，成功后再启动和做完整收发验收。

不要在升级或回滚时运行两个服务副本，也不要让 systemd 与 Docker 同时打开同一数据库。

## 队列与投递故障

外域邮件进入 SQLite 队列，后台 worker 默认每 30 秒扫描，最多尝试 12 次；失败细节可在管理员队列和 journal 中查看。不要编辑 SQLite 强行改队列状态。

常见检查顺序：

1. `BRCLIO_RELAY_ADDR`、用户名、secret 文件和 implicit TLS 模式是否与提供商一致；
2. 主机 DNS 与 relay 端口是否可达；
3. smarthost 是否允许当前 envelope/header From 域；
4. SPF 是否授权真实出口，DKIM/DMARC 是否对齐；
5. 提供商配额、退信和滥用告警。

当前没有完整 DSN/bounce 和自动退信分类。队列长期异常时，先停止新增出站、保留日志和备份，再调查根因。

## 容量语义

- **用户 quota**：按该用户当前可见邮箱副本的逻辑 MIME 字节累计；用户删除/EXPUNGE 后自己的用量可下降。
- **全局归档/SQLite cap**：对 raw MIME、解码附件 BLOB、正文/全文索引等做保守物理估算，包含未删除草稿；已收发或非草稿导入的邮件不会因用户删除而降低占用。
- **磁盘低水位**：数据库所在卷可用空间低于 `BRCLIO_MIN_FREE_DISK_BYTES` 时拒收，默认 1 GiB。

这些都不是文件系统硬配额，也不能精确预测 WAL、页面碎片、ACME cache、恢复安全副本和手工备份增长。对 systemd 的 `/var/lib/brclio-mail` 或 Docker 的 `/data` 做外部容量监控、趋势告警和恢复演练；不要把低水位设为 0 来掩盖空间不足。

## 事件处理最低动作

若怀疑账号、管理员、DKIM、relay secret 或数据库泄露：

- 隔离主机并保留 journal、数据库和备份，不要先清理证据；
- 撤销相关应用密码、会话和账号访问；
- 轮换 relay、setup、TLS 私钥和受影响域 DKIM selector/key；
- 检查管理员归档访问、审计、异常队列、别名、用户和 DNS 变更；
- 根据适用法律与承诺评估通知义务。

当前自动化不能完成上述全部动作，部分需要 DNS、CA、smarthost 和云服务商控制台配合。

## Docker Compose 运维对照

Docker 是可选部署方式。日常检查：

```bash
curl -fsS https://mail.example.com/healthz
docker compose ps
docker compose exec -T brclio-mail brclio-mail doctor
docker compose logs --since 30m brclio-mail
```

一致性备份并复制到宿主机：

```bash
set -Eeuo pipefail
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_output="backups/brclio-mail-${stamp}.sqlite"
docker compose exec -T brclio-mail \
  brclio-mail backup "/data/backups/brclio-mail-${stamp}.sqlite"
mkdir -p backups
chmod 700 backups
[[ ! -e "$backup_output" && ! -L "$backup_output" ]] || {
  printf 'refusing to overwrite backup output: %s\n' "$backup_output" >&2
  exit 1
}
docker compose cp \
  "brclio-mail:/data/backups/brclio-mail-${stamp}.sqlite" \
  "$backup_output"
[[ -f "$backup_output" && ! -L "$backup_output" ]] || {
  echo 'backup export is missing, not regular, or a symbolic link' >&2
  exit 1
}
chmod 600 "$backup_output"
integrity_result="$(sqlite3 "$backup_output" 'PRAGMA integrity_check;')"
[[ "$integrity_result" == "ok" ]] || {
  printf 'integrity_check failed: %s\n' "$integrity_result" >&2
  exit 1
}
foreign_key_violations="$(sqlite3 "$backup_output" 'PRAGMA foreign_key_check;')"
[[ -z "$foreign_key_violations" ]] || {
  printf 'foreign_key_check failed:\n%s\n' "$foreign_key_violations" >&2
  exit 1
}
sha256sum "$backup_output"
```

导出文件和 `/data/backups/` 内快照是两份完整数据，不受邮箱归档配额限制。对宿主和 named volume 都做空间告警和保留策略。只有宿主副本与异地加密副本的 hash 均验证后，才删除精确的 volume 内快照；完整的文件名白名单和删除命令见 [Docker 教程的备份章节](tutorial-docker.md#11-备份)。升级停机后快照要保留到新版本通过真实收发和恢复演练。

恢复前停止服务，验证备份，并确认没有其他运行中容器使用目标 volume：

```bash
set -Eeuo pipefail
docker compose stop -t 60 brclio-mail
docker compose ps
volume_users="$(docker ps -q --filter volume=brclio-mail-data)"
[[ -z "$volume_users" ]] || {
  printf 'volume is still used by: %s\n' "$volume_users" >&2
  exit 1
}
docker volume inspect brclio-mail-data
```

然后使用一次性 root 容器保留旧文件并替换数据库。恢复目录本身不能是符号链接，`restore_file` 只允许是 `backups/` 内不含 `/` 或 `..` 的简单 `.sqlite` 文件名：

```bash
set -Eeuo pipefail
restore_file="brclio-mail-20260825T120000Z.sqlite"
[[ "$restore_file" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.sqlite$ ]] || {
  echo "invalid restore filename" >&2
  exit 1
}
[[ "$restore_file" != *..* && "$restore_file" != */* ]] || {
  echo "restore filename must be a simple basename" >&2
  exit 1
}
[[ -d backups && ! -L backups ]] || {
  echo "backups must be a real directory" >&2
  exit 1
}
restore_directory="$(cd backups && pwd -P)"
restore_source="${restore_directory}/${restore_file}"
[[ -f "$restore_source" && ! -L "$restore_source" ]] || {
  echo "restore source is missing, not regular, or a symbolic link" >&2
  exit 1
}
if ! integrity_result="$(sqlite3 -batch -bail "$restore_source" \
  'PRAGMA integrity_check;')"; then
  echo 'sqlite3 integrity_check could not run' >&2
  exit 1
fi
[[ "$integrity_result" == "ok" ]] || {
  printf 'integrity_check failed: %s\n' "$integrity_result" >&2
  exit 1
}
if ! foreign_key_violations="$(sqlite3 -batch -bail "$restore_source" \
  'PRAGMA foreign_key_check;')"; then
  echo 'sqlite3 foreign_key_check could not run' >&2
  exit 1
fi
[[ -z "$foreign_key_violations" ]] || {
  printf 'foreign_key_check failed:\n%s\n' "$foreign_key_violations" >&2
  exit 1
}

docker compose stop -t 60 brclio-mail
volume_users="$(docker ps -q --filter volume=brclio-mail-data)"
[[ -z "$volume_users" ]] || {
  printf 'volume became active again; refusing restore: %s\n' \
    "$volume_users" >&2
  exit 1
}

mapfile -t restore_helper_refs < <(docker compose config --images)
[[ "${#restore_helper_refs[@]}" -eq 1 ]] || {
  echo 'expected exactly one reviewed Compose image' >&2
  exit 1
}
restore_helper_image="$(docker image inspect -f '{{.Id}}' \
  "${restore_helper_refs[0]}")"
[[ "$restore_helper_image" == sha256:* ]] || {
  echo 'reviewed local restore helper image is missing' >&2
  exit 1
}

docker run --pull never --rm --network none --read-only --pids-limit 64 \
  --user 0 --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE \
  --cap-add FOWNER --security-opt no-new-privileges \
  -e RESTORE_FILE="$restore_file" \
  -v brclio-mail-data:/data \
  -v "${restore_directory}:/restore:ro" \
  --entrypoint /bin/sh "$restore_helper_image" -ceu '
    case "${RESTORE_FILE}" in
      "" | */* | *..*) echo "unsafe restore filename" >&2; exit 1 ;;
    esac
    restore_source="/restore/${RESTORE_FILE}"
    test -f "${restore_source}"
    test ! -L "${restore_source}"
    safety_dir="$(mktemp -d /data/restore-safety.XXXXXXXX)"
    chmod 0700 "${safety_dir}"
    for file in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
      live_path="/data/${file}"
      if [ -e "${live_path}" ] || [ -L "${live_path}" ]; then
        mv "${live_path}" "${safety_dir}/${file}"
      fi
    done
    for file in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
      test ! -e "/data/${file}"
      test ! -L "/data/${file}"
    done
    cp "${restore_source}" /data/brclio-mail.db
    chown 10001:10001 /data/brclio-mail.db
    chmod 0600 /data/brclio-mail.db
    printf "old database files preserved in %s\n" "${safety_dir}"
  '
restore_doctor_output="$(docker compose run --pull never --rm --no-deps \
  brclio-mail doctor)"
printf '%s\n' "$restore_doctor_output"
grep -F '"deliveryMode":"smarthost"' <<<"$restore_doctor_output"
if ! docker compose up --pull never -d --no-build --wait --wait-timeout 120 \
  brclio-mail; then
  docker compose logs --tail 100 brclio-mail
  docker compose stop -t 60 brclio-mail
  echo 'restored container failed readiness and was stopped' >&2
  exit 1
fi
if ! curl -fsS https://mail.example.com/healthz; then
  docker compose stop -t 60 brclio-mail
  echo 'restored service failed health check and was stopped' >&2
  exit 1
fi
```

Compose 升级必须完整执行 [Docker 教程的安全升级流程](tutorial-docker.md#12-安全升级)。该流程会先记录匹配的旧 commit、Compose、`.env`、版本和本地镜像，且在旧服务仍在线时构建新镜像；只在停止旧服务后，才由该旧镜像+旧配置生成最终一致快照。快照导出、SQLite/FK 和 SHA-256 全部通过后才运行新 doctor，doctor 又是启动硬门禁。不要从本页摘取几条命令自行拼接升级。

### Docker 升级失败回滚

以下流程只适用于新版本尚未接收新邮件的失败阶段。若新服务已经开放并可能写入新邮件，先隔离、保全现场并评估增量数据，不能直接覆盖成旧快照。

读取安全升级步骤生成的 `backups/upgrade-rollback-<timestamp>.txt`，不要直接 `source` 它；把八个值逐字填入下面开头。`rollback_backup` 必须是映射文件中记录的**停止旧服务后**快照，不是更早的在线灾备。先切回旧 Git commit 并与保存的 Compose 比对，再恢复旧 `.env` 并确认 Compose 解析到保留的旧镜像：

```bash
set -Eeuo pipefail
rollback_git_commit='0000000000000000000000000000000000000000'
rollback_image='brclio-mail:rollback-YYYYMMDDTHHMMSSZ'
rollback_image_id='sha256:0000000000000000000000000000000000000000000000000000000000000000'
rollback_version='X.Y.Z'
rollback_backup='/absolute/repo/backups/pre-upgrade-YYYYMMDDTHHMMSSZ.sqlite'
rollback_sha256='0000000000000000000000000000000000000000000000000000000000000000'
rollback_env='/absolute/repo/backups/compose-rollback-YYYYMMDDTHHMMSSZ.env'
rollback_compose='/absolute/repo/backups/docker-compose-rollback-YYYYMMDDTHHMMSSZ.yml'

[[ "$rollback_git_commit" =~ ^[0-9a-f]{40}$ && \
   "$rollback_git_commit" != '0000000000000000000000000000000000000000' && \
   "$rollback_image" != *YYYY* && "$rollback_version" != 'X.Y.Z' && \
   "$rollback_image_id" =~ ^sha256:[0-9a-f]{64}$ && \
   "$rollback_image_id" != 'sha256:0000000000000000000000000000000000000000000000000000000000000000' && \
   "$rollback_backup" != *YYYY* && "$rollback_env" != *YYYY* && \
   "$rollback_compose" != *YYYY* && \
   "$rollback_sha256" =~ ^[0-9a-f]{64}$ && \
   "$rollback_sha256" != '0000000000000000000000000000000000000000000000000000000000000000' ]] || {
  echo 'replace every rollback placeholder from the recorded mapping' >&2
  exit 1
}
[[ -f "$rollback_backup" && ! -L "$rollback_backup" && \
   -f "$rollback_env" && ! -L "$rollback_env" && \
   -f "$rollback_compose" && ! -L "$rollback_compose" ]] || {
  echo 'matching rollback snapshot, env, or Compose file is missing/unsafe' >&2
  exit 1
}
[[ -f .env && ! -L .env ]] || {
  echo 'live .env is missing or unsafe' >&2
  exit 1
}
actual_rollback_image_id="$(docker image inspect -f '{{.Id}}' \
  "$rollback_image")"
[[ "$actual_rollback_image_id" == "$rollback_image_id" ]] || {
  echo 'rollback image tag no longer points to the recorded image ID' >&2
  exit 1
}
actual_rollback_sha256="$(sha256sum "$rollback_backup" | awk '{print $1}')"
[[ "$actual_rollback_sha256" == "$rollback_sha256" ]] || {
  echo 'rollback snapshot SHA-256 does not match the manifest' >&2
  exit 1
}
test -z "$(git status --porcelain)"
git cat-file -e "${rollback_git_commit}^{commit}"
git checkout --detach "$rollback_git_commit"
test "$(git rev-parse HEAD)" = "$rollback_git_commit"
cmp -s docker-compose.yml "$rollback_compose" || {
  echo 'saved rollback Compose does not match the recorded Git commit' >&2
  exit 1
}
install -m 0600 -- "$rollback_env" .env
grep -Fx "BRCLIO_IMAGE=${rollback_image}" .env
grep -Fx "BRCLIO_VERSION=${rollback_version}" .env
docker compose config --images | grep -Fx "$rollback_image"
docker compose stop -t 60 brclio-mail
volume_users="$(docker ps -q --filter volume=brclio-mail-data)"
[[ -z "$volume_users" ]] || {
  printf 'volume is still used by: %s\n' "$volume_users" >&2
  exit 1
}
```

然后回到本节上方的完整恢复代码块，把第一行 `restore_file=...` 改成 `rollback_backup` 的 basename（例如 `pre-upgrade-20260825T120000Z.sqlite`）。该代码块会再次严格验证 SQLite，把当前 DB/WAL/SHM 留在随机安全目录，使用已经固定到本机 image ID 且禁网的 helper 恢复快照，再以旧镜像运行 doctor；只有 doctor 成功才 `up`。恢复后重新完成 HTTPS、SMTP、IMAP 与真实收发验收。

Compose 使用 Docker `local` logging driver 的轮换参数；它不是集中审计。Docker socket 等同主机高权限，必须限制访问。systemd 与 Docker 间迁移时，先停止源服务，再通过经过验证的 SQLite 快照迁移，不能直接共享正在运行的数据目录。
