#!/usr/bin/env bash
set -Eeuo pipefail

HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=install-linux.sh
. "${HELPER_DIR}/install-linux.sh"

YES=0
REMOVE_CODEX_AUTH="ask"
INSTALL_ARGS=()

script_usage() {
  cat <<USAGE
Usage: ./reinstall-linux.sh [options] [install-linux.sh options]

Removes the installed Connect AI Proxy service, command symlink, and app
configuration directory, then runs a fresh Linux install.

Options:
  -y, --yes              Do not ask for confirmation
  --dry-run              Print actions without changing the system
  --install-dir PATH     Installed app directory (default: ${DEFAULT_INSTALL_DIR})
  --remove-codex-auth    Also remove the installing user's Codex auth file
  --keep-codex-auth      Keep Codex auth without asking
  -h, --help             Show this help

All other options are forwarded to install-linux.sh.
Example:
  ./reinstall-linux.sh --server --https --domain proxy.example.com --email admin@example.com --confirm-dns
USAGE
}

parse_reinstall_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -y|--yes)
        YES=1
        shift
        ;;
      --dry-run)
        DRY_RUN=1
        INSTALL_ARGS+=("$1")
        shift
        ;;
      --install-dir)
        shift
        [[ $# -gt 0 ]] || die "--install-dir needs a path"
        INSTALL_DIR="$1"
        INSTALL_ARGS+=("--install-dir" "$1")
        shift
        ;;
      --remove-codex-auth)
        REMOVE_CODEX_AUTH="yes"
        shift
        ;;
      --keep-codex-auth)
        REMOVE_CODEX_AUTH="no"
        shift
        ;;
      -h|--help)
        script_usage
        exit 0
        ;;
      *)
        INSTALL_ARGS+=("$1")
        shift
        ;;
    esac
  done
}

confirm_or_exit() {
  local question="$1"
  local default_answer="$2"
  if [[ "${YES}" -eq 1 ]]; then
    return 0
  fi
  if ! prompt_yes_no "${question}" "${default_answer}"; then
    die "cancelled"
  fi
}

service_known() {
  command_exists systemctl || return 1
  systemctl list-unit-files "${SERVICE_NAME}" >/dev/null 2>&1 || systemctl status "${SERVICE_NAME}" >/dev/null 2>&1
}

restore_synced_settings() {
  local binary=""
  if [[ -x "/usr/local/bin/${APP_NAME}" ]]; then
    binary="/usr/local/bin/${APP_NAME}"
  elif [[ -x "${INSTALL_DIR}/${APP_NAME}" ]]; then
    binary="${INSTALL_DIR}/${APP_NAME}"
  else
    return 0
  fi
  run_sudo "${binary}" sync restore || warn "Could not restore Claude settings from the installed proxy. Continuing reinstall."
}

remove_existing_install() {
  assert_safe_install_dir
  log "Clearing existing Linux install from ${INSTALL_DIR}."
  if service_known; then
    run_sudo systemctl stop "${SERVICE_NAME}" || true
    restore_synced_settings
    run_sudo systemctl disable "${SERVICE_NAME}" || true
  else
    restore_synced_settings
  fi
  run_sudo rm -f "/etc/systemd/system/${SERVICE_NAME}"
  if command_exists systemctl; then
    run_sudo systemctl daemon-reload
  fi
  run_sudo rm -f "/usr/local/bin/${APP_NAME}"
  run_sudo rm -rf "${INSTALL_DIR}"
}

maybe_remove_codex_auth() {
  local auth_file=""
  auth_file="$(codex_auth_file)"
  [[ -n "${auth_file}" ]] || return 0
  [[ -e "${auth_file}" ]] || return 0

  if [[ "${REMOVE_CODEX_AUTH}" == "ask" ]]; then
    if prompt_yes_no "Remove Codex auth at ${auth_file} as part of the clean reinstall?" "no"; then
      REMOVE_CODEX_AUTH="yes"
    else
      REMOVE_CODEX_AUTH="no"
    fi
  fi

  if [[ "${REMOVE_CODEX_AUTH}" == "yes" ]]; then
    run_sudo rm -f "${auth_file}"
    log "Removed Codex auth at ${auth_file}."
  else
    log "Kept Codex auth at ${auth_file}."
  fi
}

main_reinstall() {
  [[ "$(uname -s)" == "Linux" ]] || die "this script is only for Linux"
  parse_reinstall_args "$@"
  require_sudo
  detect_install_user

  confirm_or_exit "This will remove ${SERVICE_NAME}, /usr/local/bin/${APP_NAME}, and ${INSTALL_DIR}, then reinstall. Continue?" "no"
  remove_existing_install
  maybe_remove_codex_auth

  log "Starting fresh install."
  run bash "${HELPER_DIR}/install-linux.sh" install "${INSTALL_ARGS[@]}"
}

main_reinstall "$@"
