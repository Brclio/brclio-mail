#!/usr/bin/env bash
set -Eeuo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary_root="$(mktemp -d "${TMPDIR:-/tmp}/brclio-mail-package-check.XXXXXX")"
cleanup() {
  if [[ -d "${temporary_root}" ]]; then
    rm -rf -- "${temporary_root}"
  fi
}
trap cleanup EXIT

binary="${temporary_root}/brclio-mail"
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "${binary}" ./cmd/brclio-mail

invalid_root="${temporary_root}/invalid-root"
if BRCLIO_INSTALL_ROOT="${invalid_root}" \
  BRCLIO_SKIP_USER_SETUP=1 \
  BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary /usr/bin/true \
  --hostname mail.invalid.test \
  --acme-email postmaster@invalid.test \
  --no-start >/dev/null 2>&1; then
  printf 'installer accepted a non-Brclio binary\n' >&2
  exit 1
fi
[[ ! -e "${invalid_root}" ]] || { printf 'invalid binary preflight left persistent installation state\n' >&2; exit 1; }

unsafe_root="${temporary_root}/unsafe-existing-root"
install -d -m 0700 "${unsafe_root}/etc/brclio-mail/secrets"
printf 'attacker-chosen-setup-token\n' >"${unsafe_root}/etc/brclio-mail/secrets/setup_token"
chmod 0644 "${unsafe_root}/etc/brclio-mail/secrets/setup_token"
if BRCLIO_INSTALL_ROOT="${unsafe_root}" \
  BRCLIO_SKIP_USER_SETUP=1 \
  BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary}" \
  --hostname mail.unsafe.test \
  --acme-email postmaster@unsafe.test \
  --no-start >/dev/null 2>&1; then
  printf 'installer accepted an unsafe existing setup token\n' >&2
  exit 1
fi
[[ ! -e "${unsafe_root}/usr/local/bin/brclio-mail" ]] || { printf 'unsafe-file preflight installed a binary\n' >&2; exit 1; }

symlink_root="${temporary_root}/symlink-existing-root"
install -d -m 0700 "${symlink_root}/etc/brclio-mail"
ln -s /dev/null "${symlink_root}/etc/brclio-mail/brclio-mail.env"
if BRCLIO_INSTALL_ROOT="${symlink_root}" \
  BRCLIO_SKIP_USER_SETUP=1 \
  BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary}" \
  --hostname mail.symlink.test \
  --acme-email postmaster@symlink.test \
  --no-start >/dev/null 2>&1; then
  printf 'installer accepted a symlink environment file\n' >&2
  exit 1
fi
[[ ! -e "${symlink_root}/usr/local/bin/brclio-mail" ]] || { printf 'symlink-file preflight installed a binary\n' >&2; exit 1; }

escaped_email_root="${temporary_root}/escaped-email-root"
BRCLIO_INSTALL_ROOT="${escaped_email_root}" \
BRCLIO_SKIP_USER_SETUP=1 \
BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary}" \
  --hostname mail.escaped.test \
  --acme-email 'first&last@example.test' \
  --no-start >/dev/null
grep -Fx 'BRCLIO_ACME_EMAIL=first&last@example.test' \
  "${escaped_email_root}/etc/brclio-mail/brclio-mail.env" >/dev/null || {
  printf 'installer corrupted a sed-sensitive ACME email\n' >&2
  exit 1
}

BRCLIO_INSTALL_ROOT="${temporary_root}/root" \
BRCLIO_SKIP_USER_SETUP=1 \
BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary}" \
  --hostname mail.company.test \
  --acme-email postmaster@company.test \
  --no-start

staged_root="${temporary_root}/root"
environment_file="${staged_root}/etc/brclio-mail/brclio-mail.env"
setup_token="${staged_root}/etc/brclio-mail/secrets/setup_token"
relay_password="${staged_root}/etc/brclio-mail/secrets/relay_password"
unit_file="${staged_root}/etc/systemd/system/brclio-mail.service"
backup_unit="${staged_root}/etc/systemd/system/brclio-mail-backup@.service"
installed_binary="${staged_root}/usr/local/bin/brclio-mail"
backup_helper="${staged_root}/usr/local/libexec/brclio-mail-backup-helper"

for required_file in "${environment_file}" "${setup_token}" "${relay_password}" "${unit_file}" "${backup_unit}" "${installed_binary}" "${backup_helper}"; do
  [[ -f "${required_file}" ]] || { printf 'missing staged file: %s\n' "${required_file}" >&2; exit 1; }
done

grep -Fx 'BRCLIO_HOSTNAME=mail.company.test' "${environment_file}" >/dev/null
grep -Fx 'BRCLIO_BASE_URL=https://mail.company.test' "${environment_file}" >/dev/null
grep -Fx 'BRCLIO_ACME_EMAIL=postmaster@company.test' "${environment_file}" >/dev/null
if grep -Eq '^BRCLIO_(SETUP_TOKEN|RELAY_PASSWORD)_FILE=' "${environment_file}"; then
  printf 'secret source path leaked into systemd environment template\n' >&2
  exit 1
fi
for listener in ':80' ':443' ':25' ':465' ':587' ':993'; do
  grep -F "=${listener}" "${environment_file}" >/dev/null
done
grep -Fx 'AmbientCapabilities=CAP_NET_BIND_SERVICE' "${unit_file}" >/dev/null
grep -Fx 'LoadCredential=setup_token:/etc/brclio-mail/secrets/setup_token' "${unit_file}" >/dev/null
grep -Fx 'User=brclio-mail' "${unit_file}" >/dev/null
grep -Fx 'ProtectSystem=strict' "${unit_file}" >/dev/null
grep -F '/var/backups/brclio-mail/.staging/%i.sqlite' "${backup_unit}" >/dev/null
if grep -F 'ReadWritePaths=' "${unit_file}" | grep -F '/var/backups/brclio-mail' >/dev/null; then
  printf 'network-facing service can write the backup vault\n' >&2
  exit 1
fi
[[ -s "${setup_token}" ]] || { printf 'setup token is empty\n' >&2; exit 1; }
[[ ! -s "${relay_password}" ]] || { printf 'relay password placeholder is not empty\n' >&2; exit 1; }
"${installed_binary}" version >/dev/null

file_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}
[[ "$(file_mode "${environment_file}")" == "600" ]] || { printf 'environment mode is unsafe\n' >&2; exit 1; }
[[ "$(file_mode "${setup_token}")" == "600" ]] || { printf 'setup token mode is unsafe\n' >&2; exit 1; }
[[ "$(file_mode "${staged_root}/var/backups/brclio-mail")" == "710" ]] || { printf 'backup vault mode is unsafe\n' >&2; exit 1; }
staging_mode="$(file_mode "${staged_root}/var/backups/brclio-mail/.staging")"
[[ "${staging_mode}" == "1730" || "${staging_mode}" == "730" ]] || { printf 'backup staging mode is unsafe\n' >&2; exit 1; }
[[ -k "${staged_root}/var/backups/brclio-mail/.staging" ]] || { printf 'backup staging sticky bit is missing\n' >&2; exit 1; }
[[ "$(file_mode "${staged_root}/var/backups/brclio-mail/.incoming")" == "700" ]] || { printf 'backup incoming mode is unsafe\n' >&2; exit 1; }

configuration_checksum_before="$(shasum -a 256 "${environment_file}" | awk '{print $1}')"
secret_checksum_before="$(shasum -a 256 "${setup_token}" | awk '{print $1}')"
BRCLIO_INSTALL_ROOT="${temporary_root}/root" \
BRCLIO_SKIP_USER_SETUP=1 \
BRCLIO_SKIP_SYSTEMCTL=1 \
  "${repository_root}/scripts/install-systemd.sh" \
  --binary "${binary}" \
  --hostname ignored.company.test \
  --acme-email ignored@company.test \
  --no-start >/dev/null
configuration_checksum_after="$(shasum -a 256 "${environment_file}" | awk '{print $1}')"
secret_checksum_after="$(shasum -a 256 "${setup_token}" | awk '{print $1}')"
[[ "${configuration_checksum_before}" == "${configuration_checksum_after}" ]] || { printf 'installer overwrote existing configuration\n' >&2; exit 1; }
[[ "${secret_checksum_before}" == "${secret_checksum_after}" ]] || { printf 'installer overwrote existing setup token\n' >&2; exit 1; }

printf 'systemd package staging check passed\n'
