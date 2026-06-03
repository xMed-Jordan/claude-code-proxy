#!/usr/bin/env bash
set -Eeuo pipefail

HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=install-linux.sh
. "${HELPER_DIR}/install-linux.sh"

YES=0
NO_SYSTEM_UPGRADE=0
SKIP_GO_LATEST=0
STATUS_FILE=""
REPO_URL="https://github.com/xMed-Jordan/claude-code-proxy.git"
BRANCH="main"
ORIGINAL_ARGS=("$@")
SERVICE_STOPPED_FOR_UPDATE=0

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
  --status-file PATH     Write machine-readable update progress for the dashboard
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
      --status-file)
        shift
        [[ $# -gt 0 ]] || die "--status-file needs a path"
        STATUS_FILE="$1"
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

update_version_from_file() {
  if [[ -f "${HELPER_DIR}/VERSION" ]]; then
    sed -n '1s/[[:space:]]//gp' "${HELPER_DIR}/VERSION" | head -n 1
  else
    printf 'unknown'
  fi
}

write_update_status() {
  [[ -n "${STATUS_FILE}" ]] || return 0
  local state="$1"
  local phase="$2"
  local message="$3"
  local version=""
  version="$(update_version_from_file)"
  python3 - "${STATUS_FILE}" "${state}" "${phase}" "${message}" "${version}" <<'PY'
import json
import os
import sys
from datetime import datetime, timezone

path, state, phase, message, version = sys.argv[1:6]
now = datetime.now(timezone.utc).isoformat()
try:
    with open(path, "r", encoding="utf-8") as handle:
        current = json.load(handle)
        if not isinstance(current, dict):
            current = {}
except Exception:
    current = {}

if state in {"queued", "running"} and not current.get("started_at"):
    current["started_at"] = now
if state in {"succeeded", "failed"}:
    current["finished_at"] = now

current.update({
    "state": state,
    "phase": phase,
    "message": message,
    "version": version,
    "updated_at": now,
})
current["running"] = state in {"queued", "running"}
directory = os.path.dirname(path)
if directory:
    os.makedirs(directory, exist_ok=True)
tmp = path + ".tmp"
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(current, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.replace(tmp, path)
PY
}

mark_update_failed() {
  local line="$1"
  write_update_status "failed" "failed" "Update failed near line ${line}. Check update-linux.sh output or journal logs."
}

recover_service_after_failed_update() {
  [[ "${SERVICE_STOPPED_FOR_UPDATE:-0}" -eq 1 ]] || return 0
  [[ "${DRY_RUN:-0}" -eq 1 ]] && return 0
  command_exists systemctl || return 0
  warn "Update failed after stopping ${SERVICE_NAME}; attempting to start it again."
  if run_sudo systemctl start "${SERVICE_NAME}"; then
    write_update_status "failed" "recovered" "Update failed, but ${SERVICE_NAME} was started again."
  else
    warn "Could not restart ${SERVICE_NAME}; run: systemctl start ${SERVICE_NAME}"
  fi
}

handle_update_error() {
  local line="$1"
  mark_update_failed "${line}"
  recover_service_after_failed_update
}

update_system_packages() {
  if [[ "${NO_SYSTEM_UPGRADE}" -eq 1 ]]; then
    log "Skipping OS package upgrade."
    return
  fi
  log "Updating OS packages through ${PKG_MANAGER}."
  # Best-effort: a broken or held host package (e.g. the apt nodejs/npm conflict
  # seen on this host) must never abort the proxy update. Tolerate a failed OS
  # upgrade with a warning and keep going so the proxy still gets rebuilt.
  run_system_package_upgrade || warn "OS package upgrade failed; continuing with the proxy update."
}

run_system_package_upgrade() {
  case "${PKG_MANAGER}" in
    apt)
      run_sudo apt-get update &&
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold &&
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get autoremove -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold
      ;;
    dnf)
      run_sudo dnf upgrade -y
      ;;
    yum)
      run_sudo yum update -y
      ;;
    zypper)
      run_sudo zypper --non-interactive refresh &&
      run_sudo zypper --non-interactive update
      ;;
    pacman)
      run_sudo pacman -Syu --noconfirm
      ;;
    apk)
      run_sudo apk update &&
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
  local current_upstream=""
  local current_gemini_key=""
  current_port="$(get_env_value "${env_file}" "PROXY_PORT" || printf '%s' "${DEFAULT_PROXY_PORT}")"
  current_public_url="$(get_env_value "${env_file}" "PROXY_PUBLIC_URL" || true)"
  if [[ -z "${current_public_url}" ]]; then
    current_public_url="$(derive_public_url_from_caddy "${current_port}" || true)"
  fi
  browser_enabled="$(get_env_value "${env_file}" "ANTIGRAVITY_BROWSER_ENABLED" || printf '0')"
  current_upstream="$(get_env_value "${env_file}" "UPSTREAM" || true)"
  current_gemini_key="$(get_env_value "${env_file}" "GEMINI_API_KEY" || true)"

  printf '%s\0' "--server" "--no-https" "--no-public-http" "--install-dir" "${INSTALL_DIR}" "--proxy-port" "${current_port}"
  if [[ -n "${current_public_url}" ]]; then
    printf '%s\0' "--public-url" "${current_public_url}"
  fi
  if [[ "${browser_enabled}" == "1" ]]; then
    printf '%s\0' "--browser-tools"
  else
    printf '%s\0' "--no-browser-tools"
  fi
  if [[ -n "${current_upstream}" ]]; then
    printf '%s\0' "--upstream" "${current_upstream}"
  fi
  if [[ -n "${current_gemini_key}" ]]; then
    printf '%s\0' "--gemini-api-key" "${current_gemini_key}"
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

  # Do NOT stop the service before rebuilding. install-linux.sh replaces the
  # binary atomically (temp file + mv) and performs a single `systemctl restart`
  # at the very end, so the proxy keeps serving during the build and is never
  # left stopped if an earlier step (apt/go/npm/build) fails. Stopping it up
  # front is exactly what left the live service dead after a failed update.
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

wait_for_url() {
  local label="$1"
  local url="$2"
  local timeout_seconds="${3:-120}"
  local deadline=$((SECONDS + timeout_seconds))
  local attempt=1
  while (( SECONDS < deadline )); do
    if run curl -fsS "${url}" >/dev/null; then
      log "${label} health check passed."
      return 0
    fi
    if [[ "${DRY_RUN}" -eq 1 ]]; then
      return 0
    fi
    write_update_status "running" "health" "Waiting for ${label} endpoint to come back online (attempt ${attempt})."
    sleep 2
    attempt=$((attempt + 1))
  done
  die "${label} health check did not pass within ${timeout_seconds}s: ${url}"
}

run_health_checks() {
  local env_file="${INSTALL_DIR}/.env"
  local current_port=""
  local current_public_url=""
  current_port="$(get_env_value "${env_file}" "PROXY_PORT" || printf '%s' "${DEFAULT_PROXY_PORT}")"
  current_public_url="$(get_env_value "${env_file}" "PROXY_PUBLIC_URL" || true)"
  command_exists curl || return
  log "Checking local health endpoint."
  wait_for_url "Local" "http://127.0.0.1:${current_port}/health" 120
  if [[ -n "${current_public_url}" ]]; then
    log "Checking public health endpoint: ${current_public_url}/health"
    wait_for_url "Public" "${current_public_url}/health" 120
  fi
}

main_update() {
  [[ "$(uname -s)" == "Linux" ]] || die "this script is only for Linux"
  parse_update_args "$@"
  trap 'handle_update_error "${LINENO}"' ERR
  write_update_status "running" "starting" "Preparing update."
  require_sudo
  detect_install_user
  detect_os_and_package_manager

  confirm_or_exit "Update system packages, pull latest code, update runtimes, rebuild, and restart ${APP_NAME}?" "yes"
  write_update_status "running" "dependencies" "Checking base dependencies."
  install_base_dependencies
  write_update_status "running" "repository" "Pulling latest code from GitHub."
  update_repository
  write_update_status "running" "system" "Updating system packages."
  update_system_packages
  write_update_status "running" "runtimes" "Updating Go, Codex, and Claude Code runtimes."
  update_go_runtime
  update_global_clis
  write_update_status "running" "installing" "Rebuilding and reinstalling the proxy."
  reinstall_proxy_from_current_checkout
  write_update_status "running" "caddy" "Reloading HTTPS reverse proxy."
  reload_caddy_if_present
  write_update_status "running" "health" "Waiting for proxy health checks."
  run_health_checks
  write_update_status "succeeded" "done" "Update complete."
  log "Update complete."
}

main_update "$@"
