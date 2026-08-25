# 运维、备份与恢复

## 日常健康检查

Compose 的容器健康检查只验证容器内 8443 TCP listener 是否存在，它不是数据库、证书或端到端邮件投递检查。至少同时监控：

```bash
curl -fsS https://mail.example.com/healthz
docker compose ps
docker compose exec -T brclio-mail brclio-mail doctor
docker compose logs --since 30m brclio-mail
```

`doctor` 打开当前数据库并执行 `PRAGMA integrity_check` 和 `PRAGMA foreign_key_check`，同时报告 SQLite 版本、数据库路径、初始化状态、用户数、TLS 模式与投递模式。它返回非零退出码就应告警。

主应用和队列通过标准输出写结构化 JSON 日志；底层协议库或 Go runtime 的少量诊断不保证采用同一 JSON 结构，也可能进入标准错误。Compose 使用 Docker `local` logging driver，并把单文件限制为 10 MiB、最多 5 个文件。日志轮换不是集中审计或告警系统；需要按组织要求同时收集 stdout/stderr、转发并限制读取权限。不要把邮件正文、secret 或完整认证失败输入复制到工单。

还应监控：

- 数据卷和宿主机剩余空间、inode 与 I/O 错误；默认 `BRCLIO_MIN_FREE_DISK_BYTES=1073741824` 会在数据库卷低于 1 GiB 可用空间时拒绝新邮件，但不能代替告警；
- 管理台队列中持续重试/失败的收件人域；
- 管理员归档保守物理估算与 `BRCLIO_MAX_ARCHIVE_BYTES`；默认 100 GiB，估算包含 raw MIME、解码附件 BLOB、正文/全文索引开销；
- 证书到期时间；
- 外部网络对 25/443/465/587/993 的可达性；
- `dmarc@` 和 `tlsrpt@` 邮箱中的报告。

当前版本没有 Prometheus/OpenTelemetry 指标、内置告警或 DMARC/TLS-RPT 报表解析。

## 在线备份

不要用 `cp` 直接复制运行中的 `.db`、`-wal`、`-shm` 文件。应用的 `backup` CLI 使用 SQLite 一致性快照并在交付前执行完整性与外键校验；SQLite 的在线备份原理见[官方备份文档](https://www.sqlite.org/backup.html)。目标文件不能已存在。

```bash
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
docker compose exec -T brclio-mail \
  brclio-mail backup "/data/backups/brclio-mail-${stamp}.sqlite"

mkdir -p backups
chmod 700 backups
docker compose cp \
  "brclio-mail:/data/backups/brclio-mail-${stamp}.sqlite" \
  "backups/brclio-mail-${stamp}.sqlite"
chmod 600 "backups/brclio-mail-${stamp}.sqlite"
```

在有 `sqlite3` 的隔离主机复核：

```bash
sqlite3 "backups/brclio-mail-${stamp}.sqlite" \
  'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

预期第一行是 `ok`，且 `foreign_key_check` 没有输出。确认离线加密副本可读后，应按保留策略移走或删除 `/data/backups/` 内的在线副本；长期把备份留在同一 volume 会同时消耗恢复余量并触发磁盘低水位。

数据库备份包含邮件原始 MIME、附件、用户与别名、密码哈希、会话、队列、审计日志、管理员归档和 DKIM 私钥。它是高度敏感的完整邮件副本：

- 备份落地后立即使用组织批准的工具加密；
- 至少保留一个不同故障域的离线/异地副本；
- 限制人员和自动化账号访问，并记录取用；
- 制定与管理员归档披露一致的保留/销毁期限；
- 定期在隔离环境恢复，不能只验证“文件存在”。

数据库快照不包含 `.env`、setup/relay secret、静态 TLS 私钥和主机防火墙配置；它们需要独立加密备份。自动 ACME 缓存在 `/data/acme`，通常可以重新申请，但受 CA 速率限制，迁移时保留整个 data volume 更稳妥。

## 恢复演练

恢复会中断 Web、SMTP 和 IMAP。先确认备份文件名和 Compose volume，绝不能把命令指向不相关的卷。

1. 为当前实例再做一份在线备份并复制出主机。
2. 用上面的 `sqlite3` 命令验证待恢复文件。
3. 停止服务，并确认没有容器使用数据卷：

```bash
docker compose stop brclio-mail
docker compose ps
docker volume inspect brclio-mail-data
```

4. 将待恢复文件放在仓库下的 `backups/`，设定简单文件名，然后使用一次性容器保存旧文件并替换数据库：

```bash
restore_file=brclio-mail-20260825T120000Z.sqlite
restore_id="$(date -u +%Y%m%dT%H%M%SZ)"

docker run --rm --user 0 \
  -e RESTORE_FILE="${restore_file}" \
  -e RESTORE_ID="${restore_id}" \
  -v brclio-mail-data:/data \
  -v "${PWD}/backups:/restore:ro" \
  alpine:3.22 sh -ceu '
    test -f "/restore/${RESTORE_FILE}"
    mkdir -m 0700 "/data/restore-safety-${RESTORE_ID}"
    for file in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
      if [ -e "/data/${file}" ]; then
        mv "/data/${file}" "/data/restore-safety-${RESTORE_ID}/${file}"
      fi
    done
    cp "/restore/${RESTORE_FILE}" /data/brclio-mail.db
    chown 10001:10001 /data/brclio-mail.db
    chmod 0600 /data/brclio-mail.db
  '
```

只允许 `restore_file` 是 `backups/` 目录中的简单文件名；不要传入路径或通配符。上述安全目录留在同一数据卷，确认恢复稳定并另有离线副本后再按保留策略处理。

5. 启动并验收：

```bash
docker compose up -d brclio-mail
docker compose exec -T brclio-mail brclio-mail doctor
curl -fsS https://mail.example.com/healthz
```

随后验证管理员/普通用户登录、随机邮件正文与附件、本地投递、外部收发、IMAP、队列、管理员归档和审计日志。若任何一步失败，停止服务，保留现场，使用 `restore-safety-*` 和开始前的在线备份回滚。

## 升级与回滚

Preview 阶段数据库迁移可能不可向后兼容。升级前：

```bash
docker compose exec -T brclio-mail brclio-mail doctor
# 按“在线备份”创建并移出一份快照
docker compose build --pull brclio-mail
docker compose up -d brclio-mail
docker compose exec -T brclio-mail brclio-mail doctor
```

不要在升级时启动新旧两个服务副本。回滚不仅要切回旧镜像，还可能必须恢复升级前数据库快照。应先在数据副本上验证升级/回滚。

## 队列与投递故障

外域邮件进入 SQLite 队列，后台 worker 按计划重试。默认最大尝试次数为 12，扫描间隔 30 秒；失败细节可在管理员队列和结构化日志查看。不要通过编辑 SQLite 强行改队列状态。

常见检查顺序：

1. `BRCLIO_RELAY_ADDR`、用户名、密码和 implicit TLS 模式是否与提供商一致；
2. 容器 DNS 与目标端口是否可达；
3. smarthost 是否允许当前 envelope/header From 域；
4. SPF 是否授权真实出口，DKIM/DMARC 是否对齐；
5. 提供商配额、退信和滥用告警。

当前版本没有完整 DSN/bounce 处理和自动退信分类。队列长期异常时应先停止新增出站、保留日志和备份，再调查根因。

## 容量语义

- **用户 quota**：按该用户当前可见邮箱副本的逻辑 MIME 字节累计；用户删除/EXPUNGE 后自己的用量可下降。
- **全局归档/SQLite cap**：对当前存储的 raw MIME、解码附件 BLOB、正文/全文索引等做保守物理估算，包含尚未删除的草稿；已收发或非草稿导入的邮件不会因用户删除而降低占用，私有草稿最后一份副本删除后会物理清理并释放其估算占用。
- **磁盘低水位**：在接收新邮件前检查数据库所在文件系统可用空间，低于 `BRCLIO_MIN_FREE_DISK_BYTES` 时拒收，默认 1 GiB。

三者都不是文件系统硬配额，也不能准确预测 SQLite WAL、页面碎片、ACME cache、恢复安全副本和手工备份的实际增长。对 `/data` 做外部容量监控、趋势告警和恢复演练；不要把低水位阈值设置为 0 来掩盖磁盘不足。

## 事件处理最低动作

若怀疑账号、管理员、DKIM 密钥、relay secret 或数据库泄露：

- 隔离主机并保留日志/备份，不要先清理证据；
- 撤销相关应用密码、会话和账号访问；
- 轮换 relay 密码、setup secret、TLS 私钥和受影响域的 DKIM selector/key；
- 检查管理员归档访问与审计日志；
- 检查异常队列、别名、用户和 DNS 变更；
- 根据适用法律和承诺评估通知义务。

当前自动化能力不能完成上述全部动作，部分需要 DNS、CA、smarthost 和云服务商控制台配合。
