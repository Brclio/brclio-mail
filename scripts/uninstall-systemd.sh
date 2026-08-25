#!/usr/bin/env bash
set -Eeuo pipefail

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

if (($# != 0)); then
  printf 'usage: sudo ./scripts/uninstall-systemd.sh\n' >&2
  exit 1
fi
if [[ "$(uname -s)" != "Linux" || "$(id -u)" -ne 0 ]]; then
  fail "run this uninstaller as root on Linux"
fi

if systemctl is-active --quiet brclio-mail-doctor.service; then
  fail "doctor is running; wait for it or stop it explicitly before uninstalling"
fi
active_backup_units="$(systemctl list-units --type=service --state=activating,running \
  --plain --no-legend 'brclio-mail-backup@*.service' 2>/dev/null || true)"
[[ -z "${active_backup_units}" ]] || fail "a Brclio Mail backup is running; wait for it before uninstalling"

was_active=false
was_enabled=false
systemctl is-active --quiet brclio-mail.service && was_active=true
systemctl is-enabled --quiet brclio-mail.service && was_enabled=true

restore_previous_availability() {
  if [[ "${was_enabled}" == "true" ]]; then
    systemctl enable brclio-mail.service >/dev/null 2>&1 || return 1
  fi
  if [[ "${was_active}" == "true" ]]; then
    systemctl start brclio-mail.service || return 1
  fi
}

uninstall_phase="pre_removal"
recover_on_exit() {
  local status="$?"
  trap - EXIT HUP INT TERM
  if ((status != 0)); then
    case "${uninstall_phase}" in
      pre_removal)
        printf 'Uninstall exited before file removal; attempting to restore previous service availability...\n' >&2
        restore_previous_availability || \
          printf 'Automatic availability restoration failed; inspect and restart brclio-mail.service manually.\n' >&2
        ;;
      removal_started)
        printf 'Uninstall exited after file removal began; no automatic restart was attempted. Preserved data and snapshots remain under /var/lib/brclio-mail and /var/backups/brclio-mail.\n' >&2
        ;;
    esac
  fi
  exit "${status}"
}
trap recover_on_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if ! systemctl disable --now brclio-mail.service 2>/dev/null; then
  if ! restore_previous_availability; then
    fail "could not stop/disable the service and could not restore its previous availability"
  fi
  uninstall_phase="complete"
  fail "could not stop/disable the service; the installation was not removed"
fi
systemctl is-active --quiet brclio-mail.service && fail "service is still active; the installation was not removed"

backup_path=""
package_snapshot_directory=""
if [[ -e /var/lib/brclio-mail/brclio-mail.db || -L /var/lib/brclio-mail/brclio-mail.db ]]; then
  [[ -f /var/lib/brclio-mail/brclio-mail.db && ! -L /var/lib/brclio-mail/brclio-mail.db ]] || \
    fail "database is not a safe regular file; refusing to remove the installed recovery tools"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  backup_path="/var/backups/brclio-mail/pre-uninstall-${stamp}.sqlite"
  backup_unit="brclio-mail-backup@pre-uninstall-${stamp}.service"
  package_snapshot_directory="/var/backups/brclio-mail/pre-uninstall-package-${stamp}"

  if ! systemctl start "${backup_unit}"; then
    if ! restore_previous_availability; then
      fail "pre-uninstall backup failed and previous service availability could not be restored"
    fi
    uninstall_phase="complete"
    fail "pre-uninstall backup failed; the installation was not removed"
  fi
  if [[ ! -f "${backup_path}" || -L "${backup_path}" || \
    "$(stat --format=%u "${backup_path}")" != "0" || "$(stat --format=%a "${backup_path}")" != "600" ]]; then
    if ! restore_previous_availability; then
      fail "no safe pre-uninstall backup was published and previous service availability could not be restored"
    fi
    uninstall_phase="complete"
    fail "no safe pre-uninstall backup was published; the installation was not removed"
  fi

  snapshot_installed_package() {
    [[ ! -e "${package_snapshot_directory}" && ! -L "${package_snapshot_directory}" ]] || return 1
    install -d -o root -g root -m 0700 "${package_snapshot_directory}" || return 1
    install -o root -g root -m 0755 /usr/local/bin/brclio-mail "${package_snapshot_directory}/brclio-mail" || return 1
    for unit_name in brclio-mail.service brclio-mail-doctor.service brclio-mail-backup@.service; do
      install -o root -g root -m 0644 "/etc/systemd/system/${unit_name}" \
        "${package_snapshot_directory}/${unit_name}" || return 1
    done
    install -o root -g root -m 0755 /usr/local/libexec/brclio-mail-backup-helper \
      "${package_snapshot_directory}/brclio-mail-backup-helper" || return 1
    if [[ -f /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf ]]; then
      install -o root -g root -m 0644 /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf \
        "${package_snapshot_directory}/brclio-mail-static-tls.conf" || return 1
    fi
  }
  if ! snapshot_installed_package; then
    if ! restore_previous_availability; then
      fail "package snapshot failed and previous service availability could not be restored"
    fi
    uninstall_phase="complete"
    fail "package snapshot failed; the installation was not removed"
  fi
fi

uninstall_phase="removal_started"
rm -f -- \
  /etc/systemd/system/brclio-mail.service \
  /etc/systemd/system/brclio-mail-doctor.service \
  /etc/systemd/system/brclio-mail-backup@.service \
  /etc/systemd/system/brclio-mail.service.previous \
  /etc/systemd/system/brclio-mail-doctor.service.previous \
  /etc/systemd/system/brclio-mail-backup@.service.previous
systemctl daemon-reload
systemctl reset-failed brclio-mail.service 2>/dev/null || true
rm -f -- /usr/local/bin/brclio-mail /usr/local/bin/brclio-mail.previous /usr/local/bin/brclio-mail.new
rm -f -- /usr/local/libexec/brclio-mail-backup-helper
rm -rf -- /usr/share/brclio-mail /usr/share/licenses/brclio-mail

uninstall_phase="complete"
printf 'Brclio Mail service and binaries were removed.\n'
printf 'Configuration, secrets, mail data, backups, and the service account were preserved:\n'
printf '  /etc/brclio-mail\n'
printf '  /etc/systemd/system/brclio-mail.service.d (if manually created)\n'
printf '  /etc/systemd/system/brclio-mail-doctor.service.d (if manually created)\n'
printf '  /var/lib/brclio-mail\n'
printf '  /var/backups/brclio-mail\n'
printf 'Review and back up those paths before any manual deletion.\n'
if [[ -n "${backup_path}" ]]; then
  printf 'Verified pre-uninstall database snapshot: %s\n' "${backup_path}"
  printf 'Previous package snapshot: %s\n' "${package_snapshot_directory}"
  printf 'To reinstall over the preserved data, review a target release and run:\n'
  printf '  sudo env BRCLIO_UPGRADE_SNAPSHOT=%s ./scripts/install-systemd.sh --version vX.Y.Z --no-start\n' "${backup_path}"
  printf '  sudo systemctl start brclio-mail-doctor.service\n'
  printf '  sudo systemctl enable --now brclio-mail.service\n'
fi
