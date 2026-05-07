#!/usr/bin/env bash
set -Eeuo pipefail

HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=install-linux.sh
. "${HELPER_DIR}/install-linux.sh"

YES=0
NO_SYSTEM_UPGRADE=0
SKIP_GO_LATEST=0
REPO_URL="https://github.com/xMed-Jordan/claude-code-proxy.git"
BRANCH="main"
ORIGINAL_ARGS=("$@")

script_usage() {
  cat <<USAGE
Usage: ./update-linux.sh [options]

Updates the Linux host packages, pulls the latest Connect AI Proxy from GitHub,
updates Go and the Codex/Claude npm CLIs, rebuilds the proxy, and restarts it.

Options:
  -y, --yes              Do not ask for confirmation
  --dry-run              Print actions without changing the system
  --install-dir PATH     Installed app directory (default: ${DEFAULT_INSTALL_DIR})
  --repo-url URL         Git repository to pull (default: ${REPO_URL})
  --branch NAME          Git branch to pull (default: ${BRANCH})
  --no-system-upgrade    Skip OS package upgrade
  --skip-go-latest       Keep the current Go version if it satisfies go.mod
  -h, --help             Show this help
USAGE
}

parse_update_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
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
      --repo-url)
        shift
        [[ $# -gt 0 ]] || die "--repo-url needs a URL"
        REPO_URL="$1"
        shift
        ;;
      --branch)
        shift
        [[ $# -gt 0 ]] || die "--branch needs a branch name"
        BRANCH="$1"
        shift
        ;;
      --no-system-upgrade)
        NO_SYSTEM_UPGRADE=1
        shift
        ;;
      --skip-go-latest)
        SKIP_GO_LATEST=1
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

update_system_packages() {
  if [[ "${NO_SYSTEM_UPGRADE}" -eq 1 ]]; then
    log "Skipping OS package upgrade."
    return
  fi
  log "Updating OS packages through ${PKG_MANAGER}."
  case "${PKG_MANAGER}" in
    apt)
      run_sudo apt-get update
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get upgrade -y
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get autoremove -y
      ;;
    dnf)
      run_sudo dnf upgrade -y
      ;;
    yum)
      run_sudo yum update -y
      ;;
    zypper)
      run_sudo zypper --non-interactive refresh
      run_sudo zypper --non-interactive update
      ;;
    pacman)
      run_sudo pacman -Syu --noconfirm
      ;;
    apk)
      run_sudo apk update
      run_sudo apk upgrade
      ;;
    *)
      warn "Unsupported package manager for system upgrade: ${PKG_MANAGER}"
      ;;
  esac
}

update_repository() {
  if [[ ! -d "${HELPER_DIR}/.git" ]]; then
    warn "${HELPER_DIR} is not a git checkout; skipping GitHub update."
    return
  fi

  local old_rev=""
  local new_rev=""
  local status=""
  old_rev="$(git -C "${HELPER_DIR}" rev-parse HEAD 2>/dev/null || true)"

  status="$(git -C "${HELPER_DIR}" status --porcelain)"
  if [[ -n "${status}" ]]; then
    if [[ "${DRY_RUN}" -eq 1 ]]; then
      log "Would stash local repository changes before pulling from GitHub."
    elif [[ "${YES}" -eq 1 ]] || prompt_yes_no "Local repository changes were found. Stash them before updating?" "no"; then
      run git -C "${HELPER_DIR}" stash push -u -m "${APP_NAME} auto-stash before update $(date -Iseconds)"
    else
      die "repository has local changes; stash or commit them before updating"
    fi
  fi

  log "Pulling latest ${APP_NAME} from ${REPO_URL} (${BRANCH})."
  run git -C "${HELPER_DIR}" remote set-url origin "${REPO_URL}"
  run git -C "${HELPER_DIR}" fetch origin "${BRANCH}"
  run git -C "${HELPER_DIR}" checkout "${BRANCH}"
  run git -C "${HELPER_DIR}" pull --ff-only origin "${BRANCH}"

  if [[ "${DRY_RUN}" -eq 0 ]]; then
    new_rev="$(git -C "${HELPER_DIR}" rev-parse HEAD 2>/dev/null || true)"
    if [[ -n "${old_rev}" && -n "${new_rev}" && "${old_rev}" != "${new_rev}" && "${CONNECT_AI_PROXY_UPDATE_REEXEC:-0}" != "1" ]]; then
      log "Repository changed; restarting update script from the latest checkout."
      export CONNECT_AI_PROXY_UPDATE_REEXEC=1
      exec bash "${HELPER_DIR}/update-linux.sh" "${ORIGINAL_ARGS[@]}"
    fi
  fi
}

derive_public_url_from_caddy() {
  local port="$1"
  local caddyfile="/etc/caddy/Caddyfile"
  [[ -f "${caddyfile}" ]] || return 1
  local site=""
  site="$(awk -v port="${port}" '
    /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
    /^[^[:space:]{}#][^{}]*[[:space:]]*\{/ {
      site = $1
      next
    }
    $0 ~ "reverse_proxy[[:space:]]+127\\.0\\.0\\.1:" port {
      print site
      exit
    }
  ' "${caddyfile}")"
  [[ -n "${site}" ]] || return 1
  case "${site}" in
    http://*|https://*) printf '%s' "${site}" ;;
    *) printf 'https://%s' "${site}" ;;
  esac
}

current_install_args() {
  local env_file="${INSTALL_DIR}/.env"
  local current_port=""
  local current_public_url=""
  local browser_enabled=""
  current_port="$(get_env_value "${env_file}" "PROXY_PORT" || printf '%s' "${DEFAULT_PROXY_PORT}")"
  current_public_url="$(get_env_value "${env_file}" "PROXY_PUBLIC_URL" || true)"
  if [[ -z "${current_public_url}" ]]; then
    current_public_url="$(derive_public_url_from_caddy "${current_port}" || true)"
  fi
  browser_enabled="$(get_env_value "${env_file}" "ANTIGRAVITY_BROWSER_ENABLED" || printf '0')"

  printf '%s\0' "--server" "--no-https" "--install-dir" "${INSTALL_DIR}" "--proxy-port" "${current_port}"
  if [[ -n "${current_public_url}" ]]; then
    printf '%s\0' "--public-url" "${current_public_url}"
  fi
  if [[ "${browser_enabled}" == "1" ]]; then
    printf '%s\0' "--browser-tools"
  else
    printf '%s\0' "--no-browser-tools"
  fi
}

update_go_runtime() {
  if [[ "${SKIP_GO_LATEST}" -eq 1 ]]; then
    ensure_go
    return
  fi
  log "Updating Go from official Go release metadata."
  install_go_from_official_archive "0.0.0"
  export PATH="/usr/local/go/bin:${PATH}"
}

update_global_clis() {
  ensure_node_runtime
  log "Updating Codex and Claude Code CLIs with npm."
  run_sudo npm i -g "${CODEX_NPM_PACKAGE}@latest" "${CLAUDE_CODE_NPM_PACKAGE}@latest"
}

reinstall_proxy_from_current_checkout() {
  local install_args=()
  while IFS= read -r -d '' item; do
    install_args+=("${item}")
  done < <(current_install_args)

  if command_exists systemctl; then
    run_sudo systemctl stop "${SERVICE_NAME}" || true
  fi
  log "Rebuilding and reinstalling ${APP_NAME} from the updated checkout."
  run bash "${HELPER_DIR}/install-linux.sh" install "${install_args[@]}"
}

reload_caddy_if_present() {
  if ! command_exists caddy || [[ ! -f /etc/caddy/Caddyfile ]]; then
    return
  fi
  log "Validating and reloading Caddy if it is configured."
  run_sudo caddy validate --config /etc/caddy/Caddyfile
  if command_exists systemctl; then
    run_sudo systemctl reload caddy || run_sudo systemctl restart caddy
  fi
}

run_health_checks() {
  local env_file="${INSTALL_DIR}/.env"
  local current_port=""
  local current_public_url=""
  current_port="$(get_env_value "${env_file}" "PROXY_PORT" || printf '%s' "${DEFAULT_PROXY_PORT}")"
  current_public_url="$(get_env_value "${env_file}" "PROXY_PUBLIC_URL" || true)"
  command_exists curl || return
  log "Checking local health endpoint."
  run curl -fsS "http://127.0.0.1:${current_port}/health" || warn "Local health check failed."
  if [[ -n "${current_public_url}" ]]; then
    log "Checking public health endpoint: ${current_public_url}/health"
    run curl -fsS "${current_public_url}/health" || warn "Public health check failed. Check Caddy logs and ACME rate limits."
  fi
}

main_update() {
  [[ "$(uname -s)" == "Linux" ]] || die "this script is only for Linux"
  parse_update_args "$@"
  require_sudo
  detect_install_user
  detect_os_and_package_manager

  confirm_or_exit "Update system packages, pull latest code, update runtimes, rebuild, and restart ${APP_NAME}?" "yes"
  install_base_dependencies
  update_repository
  update_system_packages
  update_go_runtime
  update_global_clis
  reinstall_proxy_from_current_checkout
  reload_caddy_if_present
  run_health_checks
  log "Update complete."
}

main_update "$@"
