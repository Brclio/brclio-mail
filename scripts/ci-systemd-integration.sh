#!/usr/bin/env bash
set -Eeuo pipefail

fail() {
  printf 'systemd integration: %s\n' "$*" >&2
  exit 1
}

[[ "$(uname -s)" == "Linux" && "$(id -u)" -eq 0 ]] || fail "run as root on an isolated Linux CI host"
[[ "$#" -eq 1 ]] || fail "usage: ci-systemd-integration.sh /absolute/path/to/brclio-mail"

binary_path="$1"
[[ "${binary_path}" == /* && -x "${binary_path}" ]] || fail "binary path must be absolute and executable"
command -v sqlite3 >/dev/null || fail "sqlite3 is required for rollback-content verification"
repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary_directory="$(mktemp -d /tmp/brclio-mail-systemd-ci.XXXXXX)"
ca_install_path=/usr/local/share/ca-certificates/brclio-mail-ci.crt

# GitHub's disposable hosted image deliberately makes /usr/local/bin writable
# by the runner account. A production mail host must not; normalize this
# isolated fixture instead of weakening the installer's directory checks.
chown root:root /usr/local/bin
chmod 0755 /usr/local/bin
install -d -o root -g root -m 0755 /usr/local/libexec
usr_local_bin_state="$(stat --format='%u:%g:%a' /usr/local/bin)"
usr_local_libexec_state="$(stat --format='%u:%g:%a' /usr/local/libexec)"
systemd_unit_directory_state="$(stat --format='%u:%g:%a' /etc/systemd/system)"

cleanup() {
  systemctl disable --now brclio-mail.service 2>/dev/null || true
  "${repository_root}/scripts/uninstall-systemd.sh" >/dev/null 2>&1 || true
  rm -f -- "${ca_install_path}"
  update-ca-certificates >/dev/null 2>&1 || true
  rm -rf -- "${temporary_directory}"
}
trap cleanup EXIT

wait_for_https_health() {
  local attempt

  for attempt in {1..20}; do
    if curl --fail --silent --show-error --max-time 5 --noproxy '*' \
      --resolve 'mail.example.test:443:127.0.0.1' \
      https://mail.example.test/healthz >/dev/null 2>&1; then
      return 0
    fi

    if ! systemctl is-active --quiet brclio-mail.service; then
      journalctl -u brclio-mail.service -n 100 --no-pager >&2 || true
      return 1
    fi
    sleep 1
  done

  journalctl -u brclio-mail.service -n 100 --no-pager >&2 || true
  return 1
}

"${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary_path}" \
  --hostname mail.example.test \
  --acme-email postmaster@example.test \
  --no-start

[[ "$(stat --format='%u:%g:%a' /usr/local/bin)" == "${usr_local_bin_state}" ]] || \
  fail "installer changed the existing /usr/local/bin ownership or mode"
[[ "$(stat --format='%u:%g:%a' /usr/local/libexec)" == "${usr_local_libexec_state}" ]] || \
  fail "installer changed the existing /usr/local/libexec ownership or mode"
[[ "$(stat --format='%u:%g:%a' /etc/systemd/system)" == "${systemd_unit_directory_state}" ]] || \
  fail "installer changed the existing /etc/systemd/system ownership or mode"

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -keyout "${temporary_directory}/ca.key" \
  -out "${temporary_directory}/ca.crt" \
  -subj '/CN=Brclio Mail CI CA' >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes \
  -keyout "${temporary_directory}/mail.key" \
  -out "${temporary_directory}/mail.csr" \
  -subj '/CN=mail.example.test' \
  -addext 'subjectAltName=DNS:mail.example.test' >/dev/null 2>&1
openssl x509 -req -days 1 \
  -in "${temporary_directory}/mail.csr" \
  -CA "${temporary_directory}/ca.crt" \
  -CAkey "${temporary_directory}/ca.key" \
  -CAcreateserial \
  -out "${temporary_directory}/mail.crt" \
  -extfile <(printf 'subjectAltName=DNS:mail.example.test\n') >/dev/null 2>&1

install -o root -g root -m 0600 "${temporary_directory}/mail.crt" /etc/brclio-mail/tls/fullchain.pem
install -o root -g root -m 0600 "${temporary_directory}/mail.key" /etc/brclio-mail/tls/privkey.pem
for unit_name in brclio-mail.service brclio-mail-doctor.service; do
  install -d -o root -g root -m 0755 "/etc/systemd/system/${unit_name}.d"
  install -o root -g root -m 0644 \
    /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf \
    "/etc/systemd/system/${unit_name}.d/20-static-tls.conf"
done
sed -i \
  -e 's/^BRCLIO_AUTO_TLS=.*/BRCLIO_AUTO_TLS=false/' \
  -e 's/^BRCLIO_ACME_EMAIL=.*/BRCLIO_ACME_EMAIL=/' \
  /etc/brclio-mail/brclio-mail.env

install -o root -g root -m 0644 "${temporary_directory}/ca.crt" "${ca_install_path}"
update-ca-certificates >/dev/null
systemctl daemon-reload
systemctl enable --now brclio-mail.service
systemctl is-active --quiet brclio-mail.service || fail "service is not active"
wait_for_https_health || fail "HTTPS health did not become ready after installation"
systemctl start brclio-mail-doctor.service

passwd_checksum_before="$(sha256sum /etc/passwd | awk '{print $1}')"
runuser -u brclio-mail -- ln -s /etc/passwd /var/backups/brclio-mail/.staging/ci-symlink.sqlite
if /usr/local/libexec/brclio-mail-backup-helper publish ci-symlink >/dev/null 2>&1; then
  fail "privileged backup publisher accepted a symbolic link"
fi
passwd_checksum_after="$(sha256sum /etc/passwd | awk '{print $1}')"
[[ "${passwd_checksum_before}" == "${passwd_checksum_after}" ]] || fail "symbolic-link attack changed /etc/passwd"
rm -f -- /var/backups/brclio-mail/.incoming/ci-symlink.sqlite

database_owner_before="$(stat --format=%u /var/lib/brclio-mail/brclio-mail.db)"
if runuser -u brclio-mail -- ln /var/lib/brclio-mail/brclio-mail.db \
  /var/backups/brclio-mail/.staging/ci-hardlink.sqlite 2>/dev/null; then
  if /usr/local/libexec/brclio-mail-backup-helper publish ci-hardlink >/dev/null 2>&1; then
    fail "privileged backup publisher accepted a multiply-linked file"
  fi
  [[ "$(stat --format=%u /var/lib/brclio-mail/brclio-mail.db)" == "${database_owner_before}" ]] || \
    fail "hard-link attack changed database ownership"
  rm -f -- /var/backups/brclio-mail/.incoming/ci-hardlink.sqlite
fi

backup_name="ci-manual-$(date -u +%Y%m%dT%H%M%SZ)"
systemctl start "brclio-mail-backup@${backup_name}.service"
backup_path="/var/backups/brclio-mail/${backup_name}.sqlite"
[[ -f "${backup_path}" && ! -L "${backup_path}" ]] || fail "backup was not published"
[[ "$(stat --format='%U:%G:%a' "${backup_path}")" == "root:root:600" ]] || fail "backup ownership or mode is unsafe"

main_pid="$(systemctl show --property=MainPID --value brclio-mail.service)"
[[ "${main_pid}" =~ ^[1-9][0-9]*$ ]] || fail "main service PID is invalid"
[[ "$(ps -o user= -p "${main_pid}" | xargs)" == "brclio-mail" ]] || fail "main service is not running as brclio-mail"
capability_effective="$(awk '$1 == "CapEff:" { print $2 }' "/proc/${main_pid}/status")"
capability_ambient="$(awk '$1 == "CapAmb:" { print $2 }' "/proc/${main_pid}/status")"
((16#${capability_effective} == 1024)) || fail "unexpected effective capabilities: ${capability_effective}"
((16#${capability_ambient} == 1024)) || fail "unexpected ambient capabilities: ${capability_ambient}"

for port in 25 80 443 465 587 993; do
  ss -H -ltn | awk -v suffix=":${port}" '$4 ~ suffix "$" { found=1 } END { exit !found }' || fail "port ${port} is not listening"
done
if ss -H -ltn | awk '$4 ~ /:(143|2465|2525|2587|2993|8080|8443)$/ { found=1 } END { exit !found }'; then
  fail "a disabled or container-only listener is exposed"
fi

setup_token="$(< /etc/brclio-mail/secrets/setup_token)"
if tr '\0' '\n' <"/proc/${main_pid}/environ" | grep -Fq -- "${setup_token}"; then
  fail "setup token value leaked into the process environment"
fi
tr '\0' '\n' <"/proc/${main_pid}/environ" | \
  grep -Fx 'BRCLIO_SETUP_TOKEN_FILE=/run/credentials/brclio-mail.service/setup_token' >/dev/null || \
  fail "service is not using the systemd setup credential"

if "${repository_root}/scripts/install-systemd.sh" --binary "${binary_path}" --no-start \
  >"${temporary_directory}/direct-reinstall.log" 2>&1; then
  fail "direct installer bypassed the upgrade transaction"
fi
grep -F 'use scripts/upgrade-systemd.sh' "${temporary_directory}/direct-reinstall.log" >/dev/null || \
  fail "direct installer refusal was unclear"
if BRCLIO_UPGRADE_SNAPSHOT=/var/backups/brclio-mail/pre-upgrade-20000101T000000Z.sqlite \
  "${repository_root}/scripts/install-systemd.sh" --binary "${binary_path}" --no-start \
  >"${temporary_directory}/wrong-snapshot.log" 2>&1; then
  fail "installer accepted a missing upgrade snapshot"
fi

main_pid_before_ambiguous_target="$(systemctl show --property=MainPID --value brclio-mail.service)"
if "${repository_root}/scripts/upgrade-systemd.sh" \
  --version v0.2.0-preview --binary "${binary_path}" \
  >"${temporary_directory}/ambiguous-target.log" 2>&1; then
  fail "upgrade accepted both --version and --binary"
fi
grep -F 'specify exactly one reviewed target' "${temporary_directory}/ambiguous-target.log" >/dev/null || \
  fail "ambiguous upgrade target refusal was unclear"
systemctl is-active --quiet brclio-mail.service || fail "ambiguous target validation stopped the service"
[[ "$(systemctl show --property=MainPID --value brclio-mail.service)" == "${main_pid_before_ambiguous_target}" ]] || \
  fail "ambiguous target validation restarted the service"

old_binary_checksum="$(sha256sum /usr/local/bin/brclio-mail | awk '{print $1}')"
if "${repository_root}/scripts/upgrade-systemd.sh" \
  --binary "${repository_root}/scripts/testdata/failing-doctor-brclio-mail" \
  >"${temporary_directory}/doctor-rollback.log" 2>&1; then
  fail "upgrade reported success when the replacement binary could not run doctor"
fi
grep -F 'new doctor failed; the pre-upgrade package/database were restored' \
  "${temporary_directory}/doctor-rollback.log" >/dev/null || \
  fail "doctor failure did not report a successful paired rollback"
[[ "$(sha256sum /usr/local/bin/brclio-mail | awk '{print $1}')" == "${old_binary_checksum}" ]] || \
  fail "doctor failure did not restore the old binary"
rollback_snapshots=(/var/backups/brclio-mail/pre-upgrade-*.sqlite)
[[ "${#rollback_snapshots[@]}" -eq 1 && -f "${rollback_snapshots[0]}" ]] || \
  fail "doctor rollback did not leave exactly one verified pre-upgrade snapshot"
rollback_snapshot_checksum="$(sqlite3 "${rollback_snapshots[0]}" '.dump' | sha256sum | awk '{print $1}')"
restored_database_checksum="$(sqlite3 /var/lib/brclio-mail/brclio-mail.db '.dump' | sha256sum | awk '{print $1}')"
[[ "${restored_database_checksum}" == "${rollback_snapshot_checksum}" ]] || \
  fail "doctor failure did not restore the paired pre-upgrade database"
systemctl is-active --quiet brclio-mail.service || fail "old service was not restored after doctor failure"
wait_for_https_health || fail "HTTPS health did not recover after doctor rollback"

# Avoid a same-second pre-upgrade snapshot name collision in the success case.
sleep 1

"${repository_root}/scripts/upgrade-systemd.sh" --binary "${binary_path}"
systemctl is-active --quiet brclio-mail.service || fail "service is inactive after upgrade"
wait_for_https_health || fail "HTTPS health did not become ready after upgrade"

"${repository_root}/scripts/uninstall-systemd.sh" >"${temporary_directory}/uninstall.log"
[[ ! -e /usr/local/bin/brclio-mail && -f /var/lib/brclio-mail/brclio-mail.db ]] || \
  fail "uninstall did not remove the binary while preserving mail data"
recovery_snapshot="$(awk -F': ' '$1 == "Verified pre-uninstall database snapshot" { print $2 }' \
  "${temporary_directory}/uninstall.log")"
[[ "${recovery_snapshot}" =~ ^/var/backups/brclio-mail/pre-uninstall-[0-9]{8}T[0-9]{6}Z\.sqlite$ ]] || \
  fail "uninstall did not report a valid recovery snapshot"
BRCLIO_UPGRADE_SNAPSHOT="${recovery_snapshot}" \
  "${repository_root}/scripts/install-systemd.sh" --binary "${binary_path}" --no-start
systemctl start brclio-mail-doctor.service
systemctl enable --now brclio-mail.service
systemctl is-active --quiet brclio-mail.service || fail "service is inactive after preserved-data reinstall"
wait_for_https_health || fail "HTTPS health did not become ready after preserved-data reinstall"

systemd-analyze security --no-pager brclio-mail.service | sed -n '1,12p'
printf 'bare-metal systemd integration passed\n'
