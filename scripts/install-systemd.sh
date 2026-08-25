#!/usr/bin/env bash
set -Eeuo pipefail

readonly default_version="v0.2.0-preview"
readonly service_user="brclio-mail"
readonly service_group="brclio-mail"

version="${BRCLIO_MAIL_VERSION:-${default_version}}"
binary_source=""
mail_hostname=""
acme_email=""
no_start=false
install_root="${BRCLIO_INSTALL_ROOT:-}"
skip_user_setup="${BRCLIO_SKIP_USER_SETUP:-0}"
skip_systemctl="${BRCLIO_SKIP_SYSTEMCTL:-0}"
work_dir=""
downloaded_release=false
version_option_supplied=false
binary_option_supplied=false

usage() {
  cat <<'EOF'
Install Brclio Mail directly on a systemd Linux server.

Usage:
  sudo ./scripts/install-systemd.sh \
    --hostname mail.example.com \
    --acme-email postmaster@example.com

Options:
  --hostname HOST       Public mail host whose A/AAAA points to this server.
  --acme-email EMAIL    Contact email for automatic TLS certificates.
  --version TAG         GitHub release tag to install (default: v0.2.0-preview).
  --binary PATH         Install a local binary instead of downloading a release.
  --no-start            Install files but do not enable, start, or restart service.
  -h, --help            Show this help.

Existing configuration, secrets, data, and backups are never overwritten.
When run from an extracted release package without --version/--binary, the
packaged binary is used instead of downloading another release.
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ -n "${work_dir}" && -d "${work_dir}" ]]; then
    rm -rf -- "${work_dir}"
  fi
}
trap cleanup EXIT

while (($# > 0)); do
  case "$1" in
    --hostname)
      (($# >= 2)) || fail "--hostname requires a value"
      mail_hostname="$2"
      shift 2
      ;;
    --acme-email)
      (($# >= 2)) || fail "--acme-email requires a value"
      acme_email="$2"
      shift 2
      ;;
    --version)
      (($# >= 2)) || fail "--version requires a value"
      [[ "${version_option_supplied}" == "false" ]] || fail "--version may be provided only once"
      version_option_supplied=true
      version="$2"
      shift 2
      ;;
    --binary)
      (($# >= 2)) || fail "--binary requires a value"
      [[ "${binary_option_supplied}" == "false" ]] || fail "--binary may be provided only once"
      binary_option_supplied=true
      binary_source="$2"
      shift 2
      ;;
    --no-start)
      no_start=true
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1"
      ;;
  esac
done

if [[ "${version_option_supplied}" == "true" && "${binary_option_supplied}" == "true" ]]; then
  fail "--version and --binary are mutually exclusive"
fi

[[ "${version}" =~ ^v[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || fail "invalid release tag: ${version}"
if [[ -n "${mail_hostname}" ]]; then
  ((${#mail_hostname} <= 253)) || fail "hostname is longer than 253 characters"
  [[ "${mail_hostname}" == *.* ]] || fail "hostname must be a fully qualified name"
  [[ "${mail_hostname}" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] || fail "hostname contains invalid characters"
fi
if [[ -n "${acme_email}" ]]; then
  [[ "${acme_email}" == *@*.* && "${acme_email}" != *[[:space:]\|]* ]] || fail "invalid ACME email"
fi
if [[ -n "${mail_hostname}" || -n "${acme_email}" ]]; then
  [[ -n "${mail_hostname}" && -n "${acme_email}" ]] || fail "--hostname and --acme-email must be provided together"
fi

if [[ -z "${install_root}" ]]; then
  [[ "$(uname -s)" == "Linux" ]] || fail "direct installation requires Linux"
  [[ "$(id -u)" -eq 0 ]] || fail "run this installer as root"
  command -v systemctl >/dev/null || fail "systemd is required"
  systemd_version="$(systemctl --version | awk 'NR == 1 { print $2 }')"
  [[ "${systemd_version}" =~ ^[0-9]+$ && "${systemd_version}" -ge 247 ]] || fail "systemd 247 or newer is required"
else
  [[ "${install_root}" == /* ]] || fail "BRCLIO_INSTALL_ROOT must be an absolute path"
  skip_user_setup=1
  skip_systemctl=1
fi

for command_name in install mktemp tar openssl; do
  command -v "${command_name}" >/dev/null || fail "required command not found: ${command_name}"
done

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/brclio-mail-install.XXXXXX")"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
source_root="$(cd -- "${script_dir}/.." && pwd -P)"
package_root="${source_root}"

if [[ -z "${binary_source}" && "${version_option_supplied}" == "false" && \
  -x "${source_root}/brclio-mail" && -f "${source_root}/packaging/brclio-mail.env.example" ]]; then
  binary_source="${source_root}/brclio-mail"
  printf 'Using the binary bundled with this release package.\n'
fi

if [[ -n "${binary_source}" ]]; then
  [[ -f "${binary_source}" ]] || fail "local binary not found: ${binary_source}"
  binary_source="$(cd -- "$(dirname -- "${binary_source}")" && pwd -P)/$(basename -- "${binary_source}")"
else
  downloaded_release=true
  for command_name in curl sha256sum; do
    command -v "${command_name}" >/dev/null || fail "required command not found: ${command_name}"
  done
  case "$(uname -m)" in
    x86_64 | amd64) architecture="amd64" ;;
    aarch64 | arm64) architecture="arm64" ;;
    *) fail "unsupported CPU architecture: $(uname -m)" ;;
  esac
  release_version="${version#v}"
  package_name="brclio-mail_${release_version}_linux_${architecture}"
  archive_name="${package_name}.tar.gz"
  release_url="https://github.com/Brclio/brclio-mail/releases/download/${version}"

  printf 'Downloading %s...\n' "${version}"
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
  binary_source="${package_root}/brclio-mail"
fi

unit_source="${package_root}/packaging/systemd/brclio-mail.service"
environment_template="${package_root}/packaging/brclio-mail.env.example"
[[ -f "${unit_source}" ]] || fail "systemd unit is missing: ${unit_source}"
[[ -f "${environment_template}" ]] || fail "environment template is missing: ${environment_template}"

binary_version_output="$("${binary_source}" version)" || fail "selected binary failed its version smoke test"
binary_product="$(awk 'NR == 1 { print $1 }' <<<"${binary_version_output}")"
actual_version="$(awk 'NR == 1 { print $2 }' <<<"${binary_version_output}")"
[[ "${binary_product}" == "brclio-mail" && -n "${actual_version}" ]] || \
  fail "selected binary is not Brclio Mail or returned an invalid version line"
if [[ "${downloaded_release}" == "true" ]]; then
  [[ "${actual_version}" == "${version#v}" ]] || fail "release binary version ${actual_version} does not match requested ${version}"
fi

destination() {
  printf '%s%s' "${install_root}" "$1"
}

install_directory() {
  local mode="$1" owner="$2" group="$3" path="$4"
  if [[ "${skip_user_setup}" == "1" ]]; then
    install -d -m "${mode}" "$(destination "${path}")"
  else
    install -d -o "${owner}" -g "${group}" -m "${mode}" "$(destination "${path}")"
  fi
}

install_regular_file() {
  local mode="$1" owner="$2" group="$3" source="$4" path="$5"
  if [[ "${skip_user_setup}" == "1" ]]; then
    install -m "${mode}" "${source}" "$(destination "${path}")"
  else
    install -o "${owner}" -g "${group}" -m "${mode}" "${source}" "$(destination "${path}")"
  fi
}

ensure_global_directory() {
  local path="$1" resolved_path
  resolved_path="$(destination "${path}")"
  if [[ "${skip_user_setup}" == "1" ]]; then
    install -d -m 0755 "${resolved_path}"
    return
  fi
  if [[ ! -e "${resolved_path}" && ! -L "${resolved_path}" ]]; then
    install -d -o root -g root -m 0755 "${resolved_path}"
    return
  fi
  [[ -d "${resolved_path}" && ! -L "${resolved_path}" ]] || fail "system directory is missing or unsafe: ${path}"
  [[ "$(stat --format=%u "${resolved_path}")" == "0" ]] || fail "system directory must be owned by root: ${path}"
  directory_mode="$(stat --format=%a "${resolved_path}")"
  directory_permissions=$((8#${directory_mode: -3}))
  (((directory_permissions & 0022) == 0)) || fail "system directory must not be group/other writable: ${path}"
}

portable_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

portable_size() {
  if stat -c '%s' "$1" >/dev/null 2>&1; then
    stat -c '%s' "$1"
  else
    stat -f '%z' "$1"
  fi
}

validate_sensitive_file() {
  local path="$1" require_nonempty="$2" resolved_path size
  resolved_path="$(destination "${path}")"
  if [[ ! -e "${resolved_path}" && ! -L "${resolved_path}" ]]; then
    return
  fi
  [[ -f "${resolved_path}" && ! -L "${resolved_path}" ]] || fail "sensitive file must be a regular non-symlink: ${path}"
  [[ "$(portable_mode "${resolved_path}")" == "600" ]] || fail "sensitive file mode must be 0600: ${path}"
  if [[ "${skip_user_setup}" != "1" ]]; then
    [[ "$(stat --format=%u "${resolved_path}")" == "0" && "$(stat --format=%g "${resolved_path}")" == "0" ]] || \
      fail "sensitive file must be owned by root:root: ${path}"
  fi
  size="$(portable_size "${resolved_path}")"
  [[ "${size}" =~ ^[0-9]+$ && "${size}" -le 1048576 ]] || fail "sensitive file exceeds 1 MiB: ${path}"
  if [[ "${require_nonempty}" == "true" ]]; then
    [[ "${size}" -gt 0 ]] || fail "sensitive file must not be empty: ${path}"
  fi
}

for persistent_path in \
  /etc/brclio-mail \
  /etc/brclio-mail/secrets \
  /etc/brclio-mail/tls \
  /var/lib/brclio-mail \
  /var/backups/brclio-mail \
  /var/backups/brclio-mail/.staging \
  /var/backups/brclio-mail/.incoming; do
  resolved_path="$(destination "${persistent_path}")"
  [[ ! -L "${resolved_path}" ]] || fail "persistent path must not be a symbolic link: ${persistent_path}"
  [[ ! -e "${resolved_path}" || -d "${resolved_path}" ]] || fail "persistent path is not a directory: ${persistent_path}"
done
validate_sensitive_file /etc/brclio-mail/brclio-mail.env false
validate_sensitive_file /etc/brclio-mail/secrets/setup_token true
validate_sensitive_file /etc/brclio-mail/secrets/relay_password false
validate_sensitive_file /etc/brclio-mail/tls/fullchain.pem true
validate_sensitive_file /etc/brclio-mail/tls/privkey.pem true

if [[ -z "${install_root}" ]]; then
  existing_installation=false
  for existing_path in /usr/local/bin/brclio-mail /etc/systemd/system/brclio-mail.service /var/lib/brclio-mail/brclio-mail.db; do
    if [[ -e "${existing_path}" || -L "${existing_path}" ]]; then
      existing_installation=true
    fi
  done
  if [[ -f /etc/brclio-mail/brclio-mail.env ]]; then
    grep -Fx 'BRCLIO_DATA_DIR=/var/lib/brclio-mail' /etc/brclio-mail/brclio-mail.env >/dev/null || \
      fail "existing installation uses a nonstandard data directory; migrate it explicitly"
    grep -Fx 'BRCLIO_DATABASE_PATH=/var/lib/brclio-mail/brclio-mail.db' /etc/brclio-mail/brclio-mail.env >/dev/null || \
      fail "existing installation uses a nonstandard database path; migrate it explicitly"
  fi
  if [[ "${existing_installation}" == "true" ]]; then
    upgrade_snapshot="${BRCLIO_UPGRADE_SNAPSHOT:-}"
    [[ "${upgrade_snapshot}" =~ ^/var/backups/brclio-mail/pre-(upgrade|uninstall)-[0-9]{8}T[0-9]{6}Z\.sqlite$ ]] || \
      fail "existing installation detected; use scripts/upgrade-systemd.sh or the recovery command printed by uninstall-systemd.sh"
    [[ -f "${upgrade_snapshot}" && ! -L "${upgrade_snapshot}" ]] || fail "verified upgrade snapshot is missing"
    [[ "$(stat --format=%u "${upgrade_snapshot}")" == "0" ]] || fail "upgrade snapshot must be owned by root"
    [[ "$(stat --format=%a "${upgrade_snapshot}")" == "600" ]] || fail "upgrade snapshot mode must be 0600"
    [[ "${no_start}" == "true" ]] || fail "existing installations must be staged with --no-start and verified by doctor before listeners reopen"
    if systemctl is-active --quiet brclio-mail.service; then
      fail "upgrade primitive refuses to run while the mail service is active"
    fi
  fi
fi

if [[ "${skip_user_setup}" != "1" ]]; then
  command -v getent >/dev/null || fail "getent is required"
  command -v groupadd >/dev/null || fail "groupadd is required"
  command -v useradd >/dev/null || fail "useradd is required"
  if ! getent group "${service_group}" >/dev/null; then
    groupadd --system "${service_group}"
  fi
  if ! id "${service_user}" >/dev/null 2>&1; then
    nologin_shell="$(command -v nologin || printf '/usr/sbin/nologin')"
    useradd --system --gid "${service_group}" --home-dir /var/lib/brclio-mail \
      --shell "${nologin_shell}" --comment "Brclio Mail service" "${service_user}"
  fi
  [[ "$(id -gn "${service_user}")" == "${service_group}" ]] || fail "existing ${service_user} user has an unexpected primary group"
fi

install_directory 0700 root root /etc/brclio-mail
install_directory 0700 root root /etc/brclio-mail/secrets
install_directory 0700 root root /etc/brclio-mail/tls
install_directory 0700 "${service_user}" "${service_group}" /var/lib/brclio-mail
install_directory 0710 root "${service_group}" /var/backups/brclio-mail
install_directory 1730 root "${service_group}" /var/backups/brclio-mail/.staging
install_directory 0700 root root /var/backups/brclio-mail/.incoming
ensure_global_directory /usr/local/bin
ensure_global_directory /usr/local/libexec
install_directory 0755 root root /usr/share/licenses/brclio-mail
install_directory 0755 root root /usr/share/licenses/brclio-mail/LICENSES
install_directory 0755 root root /usr/share/brclio-mail/systemd
ensure_global_directory /etc/systemd/system

if [[ "${skip_user_setup}" != "1" ]]; then
  command -v findmnt >/dev/null || fail "findmnt from util-linux is required"
  filesystem_type="$(findmnt --noheadings --output FSTYPE --target /var/lib/brclio-mail | awk 'NR == 1 { print tolower($1) }')"
  case "${filesystem_type}" in
    nfs | nfs4 | cifs | smb3 | ceph | cephfs | glusterfs | fuse*)
      fail "SQLite data directory uses unsupported network/FUSE filesystem: ${filesystem_type}"
      ;;
    "")
      fail "could not determine the data-directory filesystem"
      ;;
  esac
fi

environment_path="$(destination /etc/brclio-mail/brclio-mail.env)"
if [[ ! -e "${environment_path}" ]]; then
  generated_environment="${work_dir}/brclio-mail.env"
  cp "${environment_template}" "${generated_environment}"
  if [[ -n "${mail_hostname}" ]]; then
    sed -i.bak \
      -e "s|mail.example.com|${mail_hostname}|g" \
      -e "s|postmaster@example.com|${acme_email}|g" \
      "${generated_environment}"
    rm -f -- "${generated_environment}.bak"
  fi
  install_regular_file 0600 root root "${generated_environment}" /etc/brclio-mail/brclio-mail.env
else
  printf 'Preserving existing configuration: /etc/brclio-mail/brclio-mail.env\n'
fi

setup_secret="$(destination /etc/brclio-mail/secrets/setup_token)"
if [[ ! -e "${setup_secret}" ]]; then
  openssl rand -base64 48 >"${work_dir}/setup_token"
  install_regular_file 0600 root root "${work_dir}/setup_token" /etc/brclio-mail/secrets/setup_token
fi

relay_secret="$(destination /etc/brclio-mail/secrets/relay_password)"
if [[ ! -e "${relay_secret}" ]]; then
  : >"${work_dir}/relay_password"
  install_regular_file 0600 root root "${work_dir}/relay_password" /etc/brclio-mail/secrets/relay_password
fi

installed_binary="$(destination /usr/local/bin/brclio-mail)"
new_binary="${installed_binary}.new"
install_regular_file 0755 root root "${binary_source}" /usr/local/bin/brclio-mail.new
if [[ -f "${installed_binary}" ]]; then
  install_regular_file 0755 root root "${installed_binary}" /usr/local/bin/brclio-mail.previous
fi
mv -f -- "${new_binary}" "${installed_binary}"

for unit_name in brclio-mail.service brclio-mail-doctor.service brclio-mail-backup@.service; do
  unit_path="${package_root}/packaging/systemd/${unit_name}"
  [[ -f "${unit_path}" ]] || fail "systemd unit is missing: ${unit_path}"
  installed_unit="$(destination "/etc/systemd/system/${unit_name}")"
  if [[ -f "${installed_unit}" ]]; then
    install_regular_file 0644 root root "${installed_unit}" "/etc/systemd/system/${unit_name}.previous"
  fi
  install_regular_file 0644 root root "${unit_path}" "/etc/systemd/system/${unit_name}"
done
install_regular_file 0644 root root \
  "${package_root}/packaging/systemd/brclio-mail-static-tls.conf" \
  /usr/share/brclio-mail/systemd/brclio-mail-static-tls.conf
install_regular_file 0755 root root \
  "${package_root}/packaging/systemd/brclio-mail-backup-helper" \
  /usr/local/libexec/brclio-mail-backup-helper
for license_file in LICENSE NOTICE THIRD_PARTY_NOTICES; do
  if [[ -f "${package_root}/${license_file}" ]]; then
    install_regular_file 0644 root root "${package_root}/${license_file}" "/usr/share/licenses/brclio-mail/${license_file}"
  fi
done
if [[ -d "${package_root}/LICENSES" ]]; then
  for license_file in "${package_root}"/LICENSES/*; do
    [[ -f "${license_file}" ]] || continue
    install_regular_file 0644 root root "${license_file}" "/usr/share/licenses/brclio-mail/LICENSES/$(basename -- "${license_file}")"
  done
fi

configuration_ready=true
if grep -Eq '^(BRCLIO_HOSTNAME=mail\.example\.com|BRCLIO_ACME_EMAIL=postmaster@example\.com)$' "${environment_path}"; then
  configuration_ready=false
fi

if [[ "${skip_systemctl}" != "1" ]]; then
  command -v systemd-analyze >/dev/null || fail "systemd-analyze is required"
  systemd-analyze verify \
    brclio-mail.service \
    brclio-mail-doctor.service \
    brclio-mail-backup@package-check.service
  systemctl daemon-reload
  if [[ "${no_start}" == "false" && "${configuration_ready}" == "true" ]]; then
    if systemctl is-active --quiet brclio-mail.service; then
      systemctl restart brclio-mail.service
    else
      systemctl enable --now brclio-mail.service
    fi
    systemctl is-active --quiet brclio-mail.service || fail "service did not become active; inspect journalctl -u brclio-mail"
  else
    printf 'Service was not started. Edit /etc/brclio-mail/brclio-mail.env, then run:\n'
    printf '  sudo systemctl enable --now brclio-mail\n'
  fi
fi

printf '\nBrclio Mail installation complete.\n'
printf 'Configuration: /etc/brclio-mail/brclio-mail.env\n'
printf 'Setup token:  /etc/brclio-mail/secrets/setup_token\n'
printf 'Data:         /var/lib/brclio-mail\n'
printf 'Backups:      /var/backups/brclio-mail\n'
printf 'Logs:         journalctl -u brclio-mail\n'
