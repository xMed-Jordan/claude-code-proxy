#!/usr/bin/env bash
set -Eeuo pipefail

HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=install-linux.sh
. "${HELPER_DIR}/install-linux.sh"

YES=0
UNINSTALL_MODE="ask"
REMOVE_CADDY="ask"
REMOVE_CODEX_AUTH="ask"
REMOVE_BROWSER="ask"
INSTALLED_PROXY_PORT="${DEFAULT_PROXY_PORT}"

script_usage() {
  cat <<USAGE
Usage: ./uninstall-linux.sh [options]

Removes Connect AI Proxy from a Linux system. By default it asks whether to
remove only the app or attempt a clean uninstall of app-owned dependencies.

Options:
  --app-only             Remove only the proxy service, symlink, and app files
  --clean                Also try to remove app-owned Go, Node/npm CLIs, and Caddy
  -y, --yes              Do not ask for confirmation
  --dry-run              Print actions without changing the system
  --install-dir PATH     Installed app directory (default: ${DEFAULT_INSTALL_DIR})
  --remove-caddy         Remove Caddy even if its config is not app-dedicated
  --keep-caddy           Never remove Caddy
  --remove-browser       Try to remove Chrome/Chromium if no other use is found
  --keep-browser         Never remove Chrome/Chromium
  --remove-codex-auth    Also remove the installing user's Codex auth file
  --keep-codex-auth      Keep Codex auth without asking
  -h, --help             Show this help
USAGE
}

parse_uninstall_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --app-only)
        UNINSTALL_MODE="app"
        shift
        ;;
      --clean)
        UNINSTALL_MODE="clean"
        shift
        ;;
      -y|--yes)
        YES=1
        shift
        ;;
      --dry-run)
        DRY_RUN=1
        shift
        ;;
      --install-dir)
        shift
        [[ $# -gt 0 ]] || die "--install-dir needs a path"
        INSTALL_DIR="$1"
        shift
        ;;
      --remove-caddy)
        REMOVE_CADDY="yes"
        shift
        ;;
      --keep-caddy)
        REMOVE_CADDY="no"
        shift
        ;;
      --remove-browser)
        REMOVE_BROWSER="yes"
        shift
        ;;
      --keep-browser)
        REMOVE_BROWSER="no"
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
        die "unknown argument: $1"
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

choose_uninstall_mode() {
  if [[ "${UNINSTALL_MODE}" != "ask" ]]; then
    return
  fi
  if prompt_yes_no "Do you want a clean uninstall that also checks app-owned dependencies?" "no"; then
    UNINSTALL_MODE="clean"
  else
    UNINSTALL_MODE="app"
  fi
}

load_installed_config() {
  local env_file="${INSTALL_DIR}/.env"
  INSTALLED_PROXY_PORT="$(get_env_value "${env_file}" "PROXY_PORT" || printf '%s' "${DEFAULT_PROXY_PORT}")"
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
  run_sudo "${binary}" sync restore || warn "Could not restore Claude settings from the installed proxy."
}

remove_app_files() {
  assert_safe_install_dir
  log "Removing ${SERVICE_NAME}, command symlink, and ${INSTALL_DIR}."
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

running_process_matching() {
  local pattern="$1"
  command_exists pgrep || return 1
  pgrep -af "${pattern}" 2>/dev/null | grep -vE "(uninstall-linux\.sh|grep)" | head -n 1
}

systemd_reference_matching() {
  local pattern="$1"
  local dirs=()
  [[ -d /etc/systemd/system ]] && dirs+=(/etc/systemd/system)
  [[ -d /lib/systemd/system ]] && dirs+=(/lib/systemd/system)
  [[ -d /usr/lib/systemd/system ]] && dirs+=(/usr/lib/systemd/system)
  [[ "${#dirs[@]}" -gt 0 ]] || return 1
  grep -RIlE "${pattern}" "${dirs[@]}" 2>/dev/null | grep -v "${SERVICE_NAME}" | head -n 1
}

project_file_outside_install() {
  local pattern="$1"
  find / -xdev \
    \( -path "${HELPER_DIR}" -o -path "${HELPER_DIR}/*" -o -path "${INSTALL_DIR}" -o -path "${INSTALL_DIR}/*" \
       -o -path /proc -o -path /sys -o -path /dev -o -path /run -o -path /tmp \
       -o -path /var/cache -o -path /var/log -o -path /var/lib/caddy \) -prune \
    -o -name "${pattern}" -print -quit 2>/dev/null || true
}

package_remove() {
  [[ $# -gt 0 ]] || return 0
  case "${PKG_MANAGER}" in
    apt)
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get purge -y "$@"
      ;;
    dnf)
      run_sudo dnf remove -y "$@"
      ;;
    yum)
      run_sudo yum remove -y "$@"
      ;;
    zypper)
      run_sudo zypper --non-interactive remove "$@"
      ;;
    pacman)
      run_sudo pacman -Rns --noconfirm "$@"
      ;;
    apk)
      run_sudo apk del "$@"
      ;;
    *)
      warn "No package removal mapping for ${PKG_MANAGER}; leaving packages installed."
      return 1
      ;;
  esac
}

remove_global_clis_if_safe() {
  if ! command_exists npm; then
    warn "npm is not available; skipping global Codex and Claude CLI removal."
    return
  fi
  local blocker=""
  blocker="$(running_process_matching '(^|/)(codex|claude)([[:space:]]|$)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Codex/Claude CLI removal because a related process is running: ${blocker}"
    return
  fi
  blocker="$(systemd_reference_matching '(codex|claude)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Codex/Claude CLI removal because another service references them: ${blocker}"
    return
  fi
  log "Removing global Codex and Claude Code npm packages if they are installed."
  run_sudo npm uninstall -g "${CODEX_NPM_PACKAGE}" "${CLAUDE_CODE_NPM_PACKAGE}" || true
}

remove_go_if_safe() {
  local go_path=""
  go_path="$(command -v go 2>/dev/null || true)"
  if [[ "${go_path}" != "/usr/local/go/bin/go" ]]; then
    warn "Skipping Go removal because this script only removes /usr/local/go installs. Current go: ${go_path:-not found}"
    return
  fi
  local blocker=""
  blocker="$(running_process_matching '(^|/)go([[:space:]]|$)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Go removal because a Go process is running: ${blocker}"
    return
  fi
  blocker="$(systemd_reference_matching '(/usr/local/go|[[:space:]/]go[[:space:]])' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Go removal because another service references Go: ${blocker}"
    return
  fi
  blocker="$(project_file_outside_install 'go.mod')"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Go removal because another Go project was found: ${blocker}"
    return
  fi
  log "Removing /usr/local/go."
  run_sudo rm -rf /usr/local/go
}

remove_node_if_safe() {
  if ! command_exists node && ! command_exists npm; then
    return
  fi
  local blocker=""
  blocker="$(running_process_matching '(^|/)(node|npm|npx)([[:space:]]|$)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Node.js/npm removal because a related process is running: ${blocker}"
    return
  fi
  blocker="$(systemd_reference_matching '(^|[[:space:]/])(node|npm|npx)([[:space:]]|$)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Node.js/npm removal because another service references it: ${blocker}"
    return
  fi
  blocker="$(project_file_outside_install 'package.json')"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping Node.js/npm removal because another Node project was found: ${blocker}"
    return
  fi
  log "Removing Node.js and npm packages managed by the OS package manager."
  package_remove nodejs npm || true
}

caddyfile_dedicated_to_app() {
  local caddyfile="/etc/caddy/Caddyfile"
  [[ -f "${caddyfile}" ]] || return 1
  grep -qE "reverse_proxy[[:space:]]+127\\.0\\.0\\.1:${INSTALLED_PROXY_PORT}" "${caddyfile}" || return 1
  local site_count=""
  local reverse_count=""
  site_count="$(grep -Ec '^[^[:space:]#{}][^{}]*[[:space:]]*\{' "${caddyfile}" || true)"
  reverse_count="$(grep -Ec 'reverse_proxy[[:space:]]+' "${caddyfile}" || true)"
  [[ "${site_count}" -le 1 && "${reverse_count}" -le 1 ]]
}

remove_caddy_if_safe() {
  if [[ "${REMOVE_CADDY}" == "no" ]]; then
    log "Keeping Caddy as requested."
    return
  fi
  if ! command_exists caddy && [[ ! -f /etc/caddy/Caddyfile ]]; then
    return
  fi

  local can_remove="no"
  if [[ "${REMOVE_CADDY}" == "yes" ]]; then
    can_remove="yes"
  elif caddyfile_dedicated_to_app; then
    can_remove="yes"
  fi

  if [[ "${can_remove}" != "yes" ]]; then
    warn "Keeping Caddy because /etc/caddy/Caddyfile does not look dedicated to ${APP_NAME}."
    warn "Use --remove-caddy only if no other site on this server depends on Caddy."
    return
  fi

  if [[ -f /etc/caddy/Caddyfile ]]; then
    local backup="/etc/caddy/Caddyfile.${APP_NAME}.uninstall.$(date +%Y%m%d%H%M%S).bak"
    run_sudo cp /etc/caddy/Caddyfile "${backup}"
    log "Backed up Caddyfile to ${backup}."
  fi
  if command_exists systemctl; then
    run_sudo systemctl disable --now caddy || true
  fi
  package_remove caddy || true
}

remove_browser_if_safe() {
  if [[ "${REMOVE_BROWSER}" == "no" ]]; then
    log "Keeping Chrome/Chromium as requested."
    return
  fi
  if [[ "${REMOVE_BROWSER}" == "ask" ]]; then
    warn "Keeping Chrome/Chromium because browser packages are commonly shared. Use --remove-browser to attempt removal."
    return
  fi
  local blocker=""
  blocker="$(running_process_matching '(chrome|chromium)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping browser removal because a browser process is running: ${blocker}"
    return
  fi
  blocker="$(systemd_reference_matching '(chrome|chromium)' || true)"
  if [[ -n "${blocker}" ]]; then
    warn "Skipping browser removal because another service references it: ${blocker}"
    return
  fi
  log "Trying to remove Chrome/Chromium packages if installed by the OS package manager."
  package_remove chromium chromium-browser google-chrome-stable google-chrome || true
}

maybe_remove_codex_auth() {
  local auth_file=""
  auth_file="$(codex_auth_file)"
  [[ -n "${auth_file}" && -e "${auth_file}" ]] || return 0

  if [[ "${REMOVE_CODEX_AUTH}" == "ask" ]]; then
    if prompt_yes_no "Remove Codex auth at ${auth_file}?" "no"; then
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

clean_dependency_uninstall() {
  log "Checking optional dependencies before clean uninstall."
  remove_caddy_if_safe
  remove_global_clis_if_safe
  maybe_remove_codex_auth
  remove_go_if_safe
  remove_browser_if_safe
  remove_node_if_safe
  warn "Shared base tools such as curl, git, python3, sudo, tar, gzip, and ca-certificates are left installed."
}

main_uninstall() {
  [[ "$(uname -s)" == "Linux" ]] || die "this script is only for Linux"
  parse_uninstall_args "$@"
  require_sudo
  detect_install_user
  detect_os_and_package_manager
  load_installed_config
  choose_uninstall_mode

  if [[ "${UNINSTALL_MODE}" == "clean" ]]; then
    confirm_or_exit "Clean uninstall will remove the app and then remove only dependencies that appear unused. Continue?" "no"
  else
    confirm_or_exit "Remove only the Connect AI Proxy app files and service?" "yes"
  fi

  remove_app_files
  if [[ "${UNINSTALL_MODE}" == "clean" ]]; then
    clean_dependency_uninstall
  fi
  log "Uninstall complete."
}

main_uninstall "$@"
