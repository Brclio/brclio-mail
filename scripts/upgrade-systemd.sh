#!/usr/bin/env bash
set -Eeuo pipefail

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

[[ "$(uname -s)" == "Linux" ]] || fail "systemd upgrade requires Linux"
[[ "$(id -u)" -eq 0 ]] || fail "run this upgrade as root"
command -v systemctl >/dev/null || fail "systemd is required"
command -v curl >/dev/null || fail "curl is required for the post-start health check"

target_count=0
target_kind=""
target_value=""
arguments=("$@")
index=0
while ((index < ${#arguments[@]})); do
  case "${arguments[index]}" in
    --version | --binary)
      ((index + 1 < ${#arguments[@]})) || fail "${arguments[index]} requires a value"
      target_count=$((target_count + 1))
      target_kind="${arguments[index]#--}"
      target_value="${arguments[index + 1]}"
      index=$((index + 2))
      ;;
    --no-start)
      fail "upgrade controls service state; do not pass --no-start"
      ;;
    *)
      fail "unknown upgrade option: ${arguments[index]}"
      ;;
  esac
done
((target_count == 1)) || fail "specify exactly one reviewed target: --version TAG or --binary PATH"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
installer="${script_dir}/install-systemd.sh"
[[ -x "${installer}" ]] || fail "installer is missing or not executable: ${installer}"
for package_file in \
  "${script_dir}/../packaging/brclio-mail.env.example" \
  "${script_dir}/../packaging/systemd/brclio-mail.service" \
  "${script_dir}/../packaging/systemd/brclio-mail-doctor.service" \
  "${script_dir}/../packaging/systemd/brclio-mail-backup@.service" \
  "${script_dir}/../packaging/systemd/brclio-mail-backup-helper"; do
  [[ -f "${package_file}" ]] || fail "upgrade package input is missing: ${package_file}"
done
[[ -x /usr/local/bin/brclio-mail ]] || fail "Brclio Mail is not installed"
[[ -f /var/lib/brclio-mail/brclio-mail.db && ! -L /var/lib/brclio-mail/brclio-mail.db ]] || \
  fail "database not found or unsafe; use install-systemd.sh only for a first installation"

work_dir=""
transaction_phase="preflight"

cleanup_and_recover() {
  local status="$?"
  trap - EXIT HUP INT TERM
  if ((status != 0)); then
    case "${transaction_phase}" in
      old_stopped)
        printf 'Upgrade exited before package changes; attempting to restore old service availability...\n' >&2
        restore_old_availability || printf 'Automatic service restart failed; start brclio-mail.service manually.\n' >&2
        ;;
      package_changed)
        printf 'Upgrade exited before listeners reopened; attempting paired package/database rollback...\n' >&2
        rollback_before_start || printf 'Automatic paired rollback failed; preserve %s and %s for manual recovery.\n' \
          "${backup_path}" "${package_snapshot_directory}" >&2
        ;;
      rollback_in_progress | rollback_failed)
        printf 'Rollback was interrupted or incomplete; keep the service stopped and recover from %s and %s.\n' \
          "${backup_path}" "${package_snapshot_directory}" >&2
        ;;
      listeners_started)
        printf 'Upgrade exited after listener startup began; no stale snapshot rollback was attempted. Preserve current data and investigate.\n' >&2
        ;;
    esac
  fi
  if [[ -n "${work_dir}" && "${work_dir}" == /var/tmp/brclio-mail-upgrade.* && -d "${work_dir}" ]]; then
    rm -rf -- "${work_dir}"
  fi
  exit "${status}"
}
trap cleanup_and_recover EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

installer_arguments=()
if [[ "${target_kind}" == "version" ]]; then
  [[ "${target_value}" =~ ^v[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || fail "invalid release tag: ${target_value}"
  for command_name in mktemp tar sha256sum awk; do
    command -v "${command_name}" >/dev/null || fail "required command not found: ${command_name}"
  done
  case "$(uname -m)" in
    x86_64 | amd64) architecture="amd64" ;;
    aarch64 | arm64) architecture="arm64" ;;
    *) fail "unsupported CPU architecture: $(uname -m)" ;;
  esac
  release_version="${target_value#v}"
  package_name="brclio-mail_${release_version}_linux_${architecture}"
  archive_name="${package_name}.tar.gz"
  release_url="https://github.com/Brclio/brclio-mail/releases/download/${target_value}"
  work_dir="$(mktemp -d /var/tmp/brclio-mail-upgrade.XXXXXX)"

  printf 'Preloading and verifying %s while the old service remains available...\n' "${target_value}"
  curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 600 \
    --output "${work_dir}/${archive_name}" "${release_url}/${archive_name}"
  curl --fail --location --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 120 \
    --output "${work_dir}/checksums.txt" "${release_url}/checksums.txt"
  expected_checksum="$(awk -v name="${archive_name}" '$2 == name || $2 == "*" name { print $1 }' "${work_dir}/checksums.txt")"
  [[ "${expected_checksum}" =~ ^[0-9a-fA-F]{64}$ ]] || fail "release checksum entry is missing or invalid"
  actual_checksum="$(sha256sum "${work_dir}/${archive_name}" | awk '{print $1}')"
  [[ "${actual_checksum}" == "${expected_checksum}" ]] || fail "release checksum mismatch"
  tar -xzf "${work_dir}/${archive_name}" -C "${work_dir}"
  package_root="${work_dir}/${package_name}"
  installer="${package_root}/scripts/install-systemd.sh"
  bundled_binary="${package_root}/brclio-mail"
  [[ -x "${installer}" && -x "${bundled_binary}" ]] || fail "release package is missing its installer or binary"
  binary_version_output="$("${bundled_binary}" version)" || fail "release binary failed its version smoke test"
  binary_product="$(awk 'NR == 1 { print $1 }' <<<"${binary_version_output}")"
  actual_version="$(awk 'NR == 1 { print $2 }' <<<"${binary_version_output}")"
  [[ "${binary_product}" == "brclio-mail" && "${actual_version}" == "${release_version}" ]] || \
    fail "release binary identity/version does not match ${target_value}"
  installer_arguments=(--binary "${bundled_binary}")
else
  [[ -f "${target_value}" && -x "${target_value}" ]] || fail "local binary not found or executable: ${target_value}"
  target_value="$(cd -- "$(dirname -- "${target_value}")" && pwd -P)/$(basename -- "${target_value}")"
  binary_version_output="$("${target_value}" version)" || fail "local binary failed its version smoke test"
  binary_product="$(awk 'NR == 1 { print $1 }' <<<"${binary_version_output}")"
  actual_version="$(awk 'NR == 1 { print $2 }' <<<"${binary_version_output}")"
  [[ "${binary_product}" == "brclio-mail" && -n "${actual_version}" ]] || \
    fail "local binary is not Brclio Mail or returned an invalid version line"
  installer_arguments=(--binary "${target_value}")
fi

was_active=false
if systemctl is-active --quiet brclio-mail.service; then
  was_active=true
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_path="/var/backups/brclio-mail/pre-upgrade-${stamp}.sqlite"
backup_unit="brclio-mail-backup@pre-upgrade-${stamp}.service"
package_snapshot_directory="/var/backups/brclio-mail/pre-upgrade-package-${stamp}"
failed_state_directory="/var/backups/brclio-mail/failed-upgrade-state-${stamp}"

restore_old_availability() {
  if [[ "${was_active}" == "true" ]]; then
    systemctl start brclio-mail.service
  fi
}

transaction_phase="old_stopped"
if [[ "${was_active}" == "true" ]]; then
  printf 'Stopping Brclio Mail so the rollback snapshot has no write gap...\n'
  if ! systemctl stop brclio-mail.service; then
    if ! restore_old_availability; then
      fail "the old service stop failed and its previous availability could not be restored"
    fi
    transaction_phase="complete"
    fail "the old service could not be stopped; the installation was not changed and availability was restored"
  fi
  if systemctl is-active --quiet brclio-mail.service; then
    fail "service did not stop"
  fi
fi

printf 'Creating verified pre-upgrade backup...\n'
if ! systemctl start "${backup_unit}"; then
  if ! restore_old_availability; then
    fail "pre-upgrade backup failed and the unchanged old service could not be restarted"
  fi
  transaction_phase="complete"
  fail "pre-upgrade backup failed; the old installation was not changed"
fi
if [[ ! -f "${backup_path}" || -L "${backup_path}" ]]; then
  if ! restore_old_availability; then
    fail "the backup unit published no safe snapshot and the unchanged old service could not be restarted"
  fi
  transaction_phase="complete"
  fail "backup unit did not publish the expected snapshot; the old installation was not changed"
fi

snapshot_previous_package() {
  install -d -o root -g root -m 0700 "${package_snapshot_directory}" || return 1
  install -o root -g root -m 0755 /usr/local/bin/brclio-mail "${package_snapshot_directory}/brclio-mail" || return 1
  for unit_name in brclio-mail.service brclio-mail-doctor.service brclio-mail-backup@.service; do
    if [[ -f "/etc/systemd/system/${unit_name}" ]]; then
      install -o root -g root -m 0644 "/etc/systemd/system/${unit_name}" "${package_snapshot_directory}/${unit_name}" || return 1
    fi
  done
  if [[ -f /usr/local/libexec/brclio-mail-backup-helper ]]; then
    install -o root -g root -m 0755 /usr/local/libexec/brclio-mail-backup-helper \
      "${package_snapshot_directory}/brclio-mail-backup-helper" || return 1
  fi
  if [[ -f /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf ]]; then
    install -o root -g root -m 0644 /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf \
      "${package_snapshot_directory}/brclio-mail-static-tls.conf" || return 1
  fi
}
if ! snapshot_previous_package; then
  if ! restore_old_availability; then
    fail "the old package snapshot failed and the unchanged old service could not be restarted"
  fi
  transaction_phase="complete"
  fail "the old package snapshot failed; the old installation was not changed"
fi

rollback_before_start() {
  printf 'Restoring the pre-upgrade database and package before reopening listeners...\n' >&2
  systemctl stop brclio-mail.service 2>/dev/null || true
  systemctl stop brclio-mail-doctor.service || return 1
  systemctl is-active --quiet brclio-mail-doctor.service && return 1
  install -d -o root -g root -m 0700 "${failed_state_directory}" || return 1
  for database_entry in brclio-mail.db brclio-mail.db-wal brclio-mail.db-shm; do
    if [[ -e "/var/lib/brclio-mail/${database_entry}" || -L "/var/lib/brclio-mail/${database_entry}" ]]; then
      mv -- "/var/lib/brclio-mail/${database_entry}" "${failed_state_directory}/${database_entry}" || return 1
    fi
  done
  install -o brclio-mail -g brclio-mail -m 0600 "${backup_path}" /var/lib/brclio-mail/brclio-mail.db || return 1
  install -o root -g root -m 0755 "${package_snapshot_directory}/brclio-mail" /usr/local/bin/brclio-mail || return 1
  for unit_name in brclio-mail.service brclio-mail-doctor.service brclio-mail-backup@.service; do
    if [[ -f "${package_snapshot_directory}/${unit_name}" ]]; then
      install -o root -g root -m 0644 "${package_snapshot_directory}/${unit_name}" "/etc/systemd/system/${unit_name}" || return 1
    fi
  done
  if [[ -f "${package_snapshot_directory}/brclio-mail-backup-helper" ]]; then
    install -o root -g root -m 0755 "${package_snapshot_directory}/brclio-mail-backup-helper" \
      /usr/local/libexec/brclio-mail-backup-helper || return 1
  fi
  if [[ -f "${package_snapshot_directory}/brclio-mail-static-tls.conf" ]]; then
    install -d -o root -g root -m 0755 /usr/share/brclio-mail/systemd || return 1
    install -o root -g root -m 0644 "${package_snapshot_directory}/brclio-mail-static-tls.conf" \
      /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf || return 1
  fi
  systemctl daemon-reload || return 1
  systemctl start brclio-mail-doctor.service || return 1
  if [[ "${was_active}" == "true" ]]; then
    systemctl start brclio-mail.service || return 1
  fi
}

transaction_phase="package_changed"
if ! BRCLIO_UPGRADE_SNAPSHOT="${backup_path}" "${installer}" "${installer_arguments[@]}" --no-start; then
  transaction_phase="rollback_in_progress"
  if rollback_before_start; then
    transaction_phase="complete"
    fail "upgrade installation failed and the pre-upgrade package/database were restored"
  fi
  transaction_phase="rollback_failed"
  fail "upgrade installation failed and automatic restoration also failed; keep ${backup_path} and ${package_snapshot_directory}"
fi

printf 'Running the new binary migration and full integrity check while listeners remain closed...\n'
if ! systemctl start brclio-mail-doctor.service; then
  transaction_phase="rollback_in_progress"
  if rollback_before_start; then
    transaction_phase="complete"
    fail "new doctor failed; the pre-upgrade package/database were restored"
  fi
  transaction_phase="rollback_failed"
  fail "new doctor failed and automatic restoration also failed; keep ${backup_path} and ${package_snapshot_directory}"
fi

if [[ "${was_active}" == "true" ]]; then
  transaction_phase="listeners_started"
  if ! systemctl start brclio-mail.service; then
    fail "the upgraded service failed during start; no automatic snapshot rollback was attempted after listener startup began"
  fi
  sleep 10
  systemctl is-active --quiet brclio-mail.service || \
    fail "upgraded service did not remain active; no automatic snapshot rollback was attempted after listeners opened"

  mail_hostname="$(awk -F= '$1 == "BRCLIO_HOSTNAME" { print substr($0, index($0, "=") + 1) }' /etc/brclio-mail/brclio-mail.env)"
  https_address="$(awk -F= '$1 == "BRCLIO_HTTPS_ADDR" { print substr($0, index($0, "=") + 1) }' /etc/brclio-mail/brclio-mail.env)"
  [[ -n "${mail_hostname}" && "${https_address}" =~ ^(.*):([0-9]+)$ ]] || \
    fail "service is active but BRCLIO_HOSTNAME/BRCLIO_HTTPS_ADDR cannot be used for the health check"
  listen_host="${BASH_REMATCH[1]}"
  https_port="${BASH_REMATCH[2]}"
  https_port_number=$((10#${https_port}))
  ((https_port_number >= 1 && https_port_number <= 65535)) || fail "BRCLIO_HTTPS_ADDR contains an invalid port"
  case "${listen_host}" in
    "" | 0.0.0.0 | 127.0.0.1 | localhost) health_address=127.0.0.1 ;;
    "[::]" | "[::1]" | :: | ::1) health_address='[::1]' ;;
    *) health_address="${listen_host}" ;;
  esac
  healthy=false
  for _ in 1 2 3 4 5 6; do
    if curl --fail --silent --show-error --max-time 15 --noproxy '*' \
      --resolve "${mail_hostname}:${https_port}:${health_address}" \
      "https://${mail_hostname}:${https_port}/healthz" >/dev/null; then
      healthy=true
      break
    fi
    sleep 5
  done
  [[ "${healthy}" == "true" ]] || \
    fail "service stayed active but HTTPS health failed; preserve current data and investigate before any rollback"
fi

transaction_phase="complete"
printf 'Upgrade complete. Verified backup: %s\n' "${backup_path}"
printf 'Previous package snapshot: %s\n' "${package_snapshot_directory}"
printf 'Previous binary convenience copy: /usr/local/bin/brclio-mail.previous\n'
