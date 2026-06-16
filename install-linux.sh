#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="connect-ai-proxy"
SERVICE_NAME="${APP_NAME}.service"
DEFAULT_INSTALL_DIR="/opt/${APP_NAME}"
DEFAULT_PROXY_PORT="4000"
DEFAULT_PROXY_HOST="127.0.0.1"
DEFAULT_HTTP_PORT="80"
DEFAULT_HTTPS_PORT="443"
GO_METADATA_URL="https://go.dev/dl/?mode=json"
GO_DOWNLOAD_BASE="https://go.dev/dl"
CODEX_NPM_PACKAGE="@openai/codex"
CLAUDE_CODE_NPM_PACKAGE="@anthropic-ai/claude-code"

ACTION="install"
DRY_RUN=0
NO_START=0
NO_ENABLE=0
INSTALL_DIR="${DEFAULT_INSTALL_DIR}"
PROXY_PORT="${DEFAULT_PROXY_PORT}"
PROXY_HOST="${DEFAULT_PROXY_HOST}"
HTTP_PORT="${DEFAULT_HTTP_PORT}"
HTTPS_PORT="${DEFAULT_HTTPS_PORT}"
INSTALL_KIND="ask"
HTTPS_MODE="ask"
BROWSER_TOOLS="ask"
CLAUDE_CODE_CHOICE="ask"
UPSTREAM_CHOICE="ask"
GEMINI_API_KEY_CHOICE=""
DOMAIN=""
EMAIL=""
PROXY_PUBLIC_URL=""
DNS_CONFIRMED=0
EXPOSE_HTTP="ask"
PKG_MANAGER=""
OS_ID=""
OS_LIKE=""
SUDO=""
INSTALL_USER=""
INSTALL_GROUP=""
INSTALL_HOME=""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_DIR="${SCRIPT_DIR}"

log() {
  printf '[%s] %s\n' "${APP_NAME}" "$*"
}

warn() {
  printf '[%s] WARNING: %s\n' "${APP_NAME}" "$*" >&2
}

die() {
  printf '[%s] ERROR: %s\n' "${APP_NAME}" "$*" >&2
  exit 1
}

usage() {
  cat <<USAGE
Usage: ./install-linux.sh [action] [options]

Actions:
  install       Install dependencies, Codex, optional Claude Code, build ${APP_NAME}, and configure systemd
  start         Start ${SERVICE_NAME}
  stop          Stop ${SERVICE_NAME}
  reload        Reload ${SERVICE_NAME}
  restart       Restart ${SERVICE_NAME}
  status        Show ${SERVICE_NAME} status
  enable        Enable ${SERVICE_NAME} at startup
  disable       Disable ${SERVICE_NAME} at startup
  uninstall     Remove the systemd service and command symlink

Options:
  --dry-run              Print privileged/install commands without running them
  --no-start             Install but do not start the service
  --no-enable            Install but do not enable startup
  --install-dir PATH     Install directory (default: ${DEFAULT_INSTALL_DIR})
  --proxy-port PORT      Internal proxy port (default: ${DEFAULT_PROXY_PORT})
  --domain DOMAIN        Enable HTTPS for this domain
  --email EMAIL          ACME certificate email for HTTPS
  --https                Ask/configure HTTPS even without --domain
  --no-https             Skip HTTPS reverse proxy setup
  --http-port PORT       Public HTTP redirect/ACME port (default: ${DEFAULT_HTTP_PORT})
  --https-port PORT      Public HTTPS port (default: ${DEFAULT_HTTPS_PORT})
  --public-url URL       Public base URL shown in the control panel
  --confirm-dns          Confirm the HTTPS domain already points to this server
  --public-http          Expose the Go proxy on 0.0.0.0 without HTTPS
  --no-public-http       Keep the Go proxy local-only when HTTPS is not configured
  --browser-tools        Install/configure Chrome or Chromium browser tools
  --no-browser-tools     Skip Chrome/Chromium and browser MCP setup
  --claude-code          Install Claude Code CLI and enable the 'claude' upstream
  --no-claude-code       Skip Claude Code CLI (no 'claude' upstream)
  --server               Use server-oriented defaults
  --upstream UPSTREAM    Default upstream provider: codex or openai (default: codex)
  --local                Use local workstation defaults
  -h, --help             Show this help
USAGE
}

parse_args() {
  if [[ $# -gt 0 ]]; then
    case "$1" in
      install|start|stop|reload|restart|status|enable|disable|uninstall)
        ACTION="$1"
        shift
        ;;
    esac
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --dry-run) DRY_RUN=1 ;;
      --no-start) NO_START=1 ;;
      --no-enable) NO_ENABLE=1 ;;
      --install-dir)
        shift
        [[ $# -gt 0 ]] || die "--install-dir needs a path"
        INSTALL_DIR="$1"
        ;;
      --upstream)
        shift
        [[ $# -gt 0 ]] || die "--upstream needs a provider"
        UPSTREAM_CHOICE="$1"
        ;;
      --gemini-api-key)
        shift
        [[ $# -gt 0 ]] || die "--gemini-api-key needs a key"
        GEMINI_API_KEY_CHOICE="$1"
        ;;
      --proxy-port)
        shift
        [[ $# -gt 0 ]] || die "--proxy-port needs a port"
        PROXY_PORT="$1"
        ;;
      --domain)
        shift
        [[ $# -gt 0 ]] || die "--domain needs a domain name"
        DOMAIN="$1"
        HTTPS_MODE="yes"
        ;;
      --email)
        shift
        [[ $# -gt 0 ]] || die "--email needs an email address"
        EMAIL="$1"
        ;;
      --https) HTTPS_MODE="yes" ;;
      --no-https) HTTPS_MODE="no" ;;
      --http-port)
        shift
        [[ $# -gt 0 ]] || die "--http-port needs a port"
        HTTP_PORT="$1"
        ;;
      --https-port)
        shift
        [[ $# -gt 0 ]] || die "--https-port needs a port"
        HTTPS_PORT="$1"
        ;;
      --public-url)
        shift
        [[ $# -gt 0 ]] || die "--public-url needs a URL"
        PROXY_PUBLIC_URL="$1"
        ;;
      --confirm-dns) DNS_CONFIRMED=1 ;;
      --public-http) EXPOSE_HTTP="yes" ;;
      --no-public-http) EXPOSE_HTTP="no" ;;
      --browser-tools) BROWSER_TOOLS="yes" ;;
      --no-browser-tools) BROWSER_TOOLS="no" ;;
      --claude-code) CLAUDE_CODE_CHOICE="yes" ;;
      --no-claude-code) CLAUDE_CODE_CHOICE="no" ;;
      --server) INSTALL_KIND="server" ;;
      --local) INSTALL_KIND="local" ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
    shift
  done
}

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

run() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    printf '[dry-run] '
    printf '%q ' "$@"
    printf '\n'
    return 0
  fi
  "$@"
}

run_sudo() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    if [[ -n "${SUDO}" ]]; then
      printf '[dry-run] sudo '
    else
      printf '[dry-run] '
    fi
    printf '%q ' "$@"
    printf '\n'
    return 0
  fi
  if [[ -n "${SUDO}" ]]; then
    "${SUDO}" "$@"
  else
    "$@"
  fi
}

sudo_shell() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    if [[ -n "${SUDO}" ]]; then
      printf '[dry-run] sudo sh -c %q\n' "$*"
    else
      printf '[dry-run] sh -c %q\n' "$*"
    fi
    return 0
  fi
  if [[ -n "${SUDO}" ]]; then
    "${SUDO}" sh -c "$*"
  else
    sh -c "$*"
  fi
}

validate_port() {
  local value="$1"
  local label="$2"
  [[ "${value}" =~ ^[0-9]+$ ]] || die "${label} must be a number"
  (( value > 0 && value < 65536 )) || die "${label} must be between 1 and 65535"
}

public_url_for_port() {
  local scheme="$1"
  local domain="$2"
  local port="$3"
  if [[ "${scheme}" == "https" && "${port}" == "443" ]]; then
    printf 'https://%s' "${domain}"
  elif [[ "${scheme}" == "http" && "${port}" == "80" ]]; then
    printf 'http://%s' "${domain}"
  else
    printf '%s://%s:%s' "${scheme}" "${domain}" "${port}"
  fi
}

prompt_yes_no() {
  local question="$1"
  local default_answer="$2"
  local prompt="[y/N]"
  local answer=""
  if [[ "${default_answer}" == "yes" ]]; then
    prompt="[Y/n]"
  fi
  if [[ ! -t 0 ]]; then
    [[ "${default_answer}" == "yes" ]]
    return
  fi
  while true; do
    read -r -p "${question} ${prompt} " answer
    answer="${answer:-${default_answer}}"
    case "${answer,,}" in
      y|yes) return 0 ;;
      n|no) return 1 ;;
      *) printf 'Please answer yes or no.\n' ;;
    esac
  done
}

prompt_value() {
  local question="$1"
  local default_value="$2"
  local required="${3:-}"
  local answer=""
  if [[ ! -t 0 ]]; then
    answer="${default_value}"
  else
    if [[ -n "${default_value}" ]]; then
      read -r -p "${question} [${default_value}] " answer
      answer="${answer:-${default_value}}"
    else
      read -r -p "${question} " answer
    fi
  fi
  answer="$(printf '%s' "${answer}" | xargs)"
  if [[ "${required}" == "required" && -z "${answer}" ]]; then
    die "${question} is required"
  fi
  printf '%s' "${answer}"
}

require_sudo() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    if [[ "${EUID}" -eq 0 ]]; then
      SUDO=""
    elif command_exists sudo; then
      SUDO="sudo"
      log "Dry run: would validate sudo with 'sudo -v'."
    else
      SUDO="sudo"
      warn "Dry run: sudo is not installed; a real install would stop here."
    fi
    return
  fi
  if [[ "${EUID}" -eq 0 ]]; then
    SUDO=""
    return
  fi
  command_exists sudo || die "sudo is required. Install sudo or run this script as root."
  sudo -v || die "sudo validation failed"
  SUDO="sudo"
}

detect_install_user() {
  if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then
    INSTALL_USER="${SUDO_USER}"
  else
    INSTALL_USER="$(id -un)"
  fi
  INSTALL_GROUP="$(id -gn "${INSTALL_USER}" 2>/dev/null || id -gn)"
  INSTALL_HOME="$(getent passwd "${INSTALL_USER}" 2>/dev/null | cut -d: -f6 || true)"
  if [[ -z "${INSTALL_HOME}" ]]; then
    INSTALL_HOME="$(eval "printf '%s' ~${INSTALL_USER}" 2>/dev/null || true)"
  fi
  if [[ -z "${INSTALL_HOME}" || "${INSTALL_HOME}" == "~${INSTALL_USER}" ]]; then
    INSTALL_HOME="${HOME:-}"
  fi
  [[ -n "${INSTALL_HOME}" ]] || die "could not determine home directory for ${INSTALL_USER}"
}

detect_os_and_package_manager() {
  if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID="${ID:-}"
    OS_LIKE="${ID_LIKE:-}"
  fi
  if command_exists apt-get; then
    PKG_MANAGER="apt"
  elif command_exists dnf; then
    PKG_MANAGER="dnf"
  elif command_exists yum; then
    PKG_MANAGER="yum"
  elif command_exists zypper; then
    PKG_MANAGER="zypper"
  elif command_exists pacman; then
    PKG_MANAGER="pacman"
  elif command_exists apk; then
    PKG_MANAGER="apk"
  else
    die "unsupported Linux package manager. Expected apt, dnf, yum, zypper, pacman, or apk."
  fi
  log "Detected Linux distro '${OS_ID:-unknown}' (${OS_LIKE:-no ID_LIKE}) with package manager '${PKG_MANAGER}'."
}

package_install() {
  [[ $# -gt 0 ]] || return 0
  case "${PKG_MANAGER}" in
    apt)
      run_sudo apt-get update
      run_sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y "$@"
      ;;
    dnf)
      run_sudo dnf install -y "$@"
      ;;
    yum)
      run_sudo yum install -y "$@"
      ;;
    zypper)
      run_sudo zypper --non-interactive refresh
      run_sudo zypper --non-interactive install "$@"
      ;;
    pacman)
      run_sudo pacman -Sy --needed --noconfirm "$@"
      ;;
    apk)
      run_sudo apk add --no-cache "$@"
      ;;
    *)
      die "unsupported package manager: ${PKG_MANAGER}"
      ;;
  esac
}

package_available() {
  local package="$1"
  case "${PKG_MANAGER}" in
    apt) apt-cache show "${package}" >/dev/null 2>&1 ;;
    dnf) dnf list --available "${package}" >/dev/null 2>&1 || rpm -q "${package}" >/dev/null 2>&1 ;;
    yum) yum list available "${package}" >/dev/null 2>&1 || rpm -q "${package}" >/dev/null 2>&1 ;;
    zypper) zypper --non-interactive search --match-exact "${package}" >/dev/null 2>&1 ;;
    pacman) pacman -Si "${package}" >/dev/null 2>&1 ;;
    apk) apk search -x "${package}" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

install_base_dependencies() {
  local packages=()
  local pkg=""
  for pkg in curl tar gzip git python3; do
    if ! command_exists "${pkg}"; then
      packages+=("${pkg}")
    fi
  done

  local has_cacerts=0
  if [[ -d /etc/ssl/certs ]] || [[ -d /etc/pki/tls/certs ]]; then
    has_cacerts=1
  fi
  if [[ "${has_cacerts}" -eq 0 ]]; then
    packages+=(ca-certificates)
  fi

  if ! command_exists node; then
    packages+=(nodejs)
  fi
  if ! command_exists npm; then
    packages+=(npm)
  fi
  if [[ "${EUID}" -eq 0 ]] && ! command_exists sudo; then
    packages+=(sudo)
  fi

  if [[ ${#packages[@]} -eq 0 ]]; then
    log "All base dependencies are already installed."
    return 0
  fi

  log "Installing base dependencies if missing: ${packages[*]}"
  package_install "${packages[@]}"
}

install_media_dependencies() {
  # Tools agy uses to read rich media (images, audio, video, PDF, Office docs)
  # and to extract archives. Best-effort: a missing optional package must never
  # abort the install — the Agy media feature simply degrades.
  local packages=()
  command_exists ffmpeg || packages+=(ffmpeg)                       # ffmpeg + ffprobe (audio/video)
  command_exists pip3 || command_exists pip || packages+=(python3-pip)
  command_exists 7z || command_exists 7za || packages+=(p7zip-full) # 7z / zip
  command_exists unar || packages+=(unar)                           # rar (apt: unar)
  if [[ ${#packages[@]} -gt 0 ]]; then
    log "Installing media dependencies (best-effort): ${packages[*]}"
    package_install "${packages[@]}" || log "WARNING: some media packages failed to install; Agy media may be limited."
  fi
  # Python libraries agy shells out to for documents (installed for root, which
  # is what agy runs as). PEP 668 needs --break-system-packages on modern apt.
  local pipbin=""
  if command_exists pip3; then pipbin="pip3"; elif command_exists pip; then pipbin="pip"; fi
  if [[ -n "${pipbin}" ]]; then
    log "Installing python document libraries (best-effort)."
    run_sudo "${pipbin}" install --break-system-packages --upgrade pymupdf python-docx openpyxl pillow python-pptx \
      || run_sudo "${pipbin}" install --upgrade pymupdf python-docx openpyxl pillow python-pptx \
      || log "WARNING: python media libraries failed to install; PDF/Office reading may be limited."
  fi
}

node_major_version() {
	if ! command_exists node; then
		return 1
	fi
	local raw=""
	raw="$(node --version 2>/dev/null || true)"
	raw="${raw#v}"
	raw="${raw%%.*}"
	[[ "${raw}" =~ ^[0-9]+$ ]] || return 1
	printf '%s' "${raw}"
}

ensure_node_runtime() {
	local major=""
	if major="$(node_major_version)" && (( major >= 18 )) && command_exists npm; then
		log "Node.js ${major}.x and npm are available."
		return
	fi
	if [[ "${DRY_RUN}" -eq 1 ]]; then
		log "Would verify Node.js 18+ and npm for Claude Code/Codex CLI installs."
		return
	fi
	die "Node.js 18+ and npm are required. The distro package install did not provide a new enough Node.js runtime."
}

required_go_version() {
  local key=""
  local value=""
  while read -r key value _; do
    if [[ "${key}" == "go" && -n "${value}" ]]; then
      printf '%s' "${value}"
      return
    fi
  done < "${REPO_DIR}/go.mod"
  printf '1.25.0'
}

semver_ge() {
  local installed="${1#go}"
  local required="${2#go}"
  local i_part r_part i
  IFS=. read -r -a i_part <<< "${installed}"
  IFS=. read -r -a r_part <<< "${required}"
  for i in 0 1 2; do
    local iv="${i_part[$i]:-0}"
    local rv="${r_part[$i]:-0}"
    iv="${iv%%[^0-9]*}"
    rv="${rv%%[^0-9]*}"
    iv="${iv:-0}"
    rv="${rv:-0}"
    if (( iv > rv )); then
      return 0
    fi
    if (( iv < rv )); then
      return 1
    fi
  done
  return 0
}

installed_go_version() {
  if ! command_exists go; then
    return 1
  fi
  local raw=""
  raw="$(go version 2>/dev/null || true)"
  set -- ${raw}
  [[ $# -ge 3 ]] || return 1
  printf '%s' "${3#go}"
}

linux_go_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64' ;;
    aarch64|arm64) printf 'arm64' ;;
    *) die "unsupported CPU architecture for Go install: $(uname -m)" ;;
  esac
}

install_go_from_official_archive() {
  local required="$1"
  local arch=""
  local metadata=""
  local selected=""
  local go_version=""
  local go_file=""
  local go_sha=""
  local archive=""
  arch="$(linux_go_arch)"
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would fetch ${GO_METADATA_URL}, select a Go release >= ${required} for linux-${arch}, verify SHA256, and install under /usr/local/go."
    return 0
  fi
  metadata="$(mktemp)"
  curl -fsSL -o "${metadata}" "${GO_METADATA_URL}"
  selected="$(python3 - "${metadata}" "${required}" "${arch}" <<'PY'
import json
import sys

path, required, arch = sys.argv[1:4]

def parse(version):
    version = version.removeprefix("go")
    parts = []
    for part in version.split("."):
        digits = ""
        for ch in part:
            if ch.isdigit():
                digits += ch
            else:
                break
        parts.append(int(digits or "0"))
    while len(parts) < 3:
        parts.append(0)
    return tuple(parts[:3])

with open(path, "r", encoding="utf-8") as fh:
    releases = json.load(fh)

required_tuple = parse(required)
candidates = []
for release in releases:
    version = release.get("version", "")
    if not release.get("stable", True):
        continue
    if parse(version) < required_tuple:
        continue
    for item in release.get("files", []):
        if item.get("os") == "linux" and item.get("arch") == arch and item.get("kind") == "archive":
            candidates.append((parse(version), version, item.get("filename", ""), item.get("sha256", "")))

if not candidates:
    sys.exit(1)

candidates.sort(reverse=True)
_, version, filename, sha256 = candidates[0]
print(version, filename, sha256)
PY
)"
  read -r go_version go_file go_sha <<< "${selected}"
  [[ -n "${go_file}" && -n "${go_sha}" ]] || die "could not select a Go archive from official metadata"
  archive="$(mktemp "/tmp/${go_file}.XXXXXX")"
  log "Installing ${go_version} from ${GO_DOWNLOAD_BASE}/${go_file}."
  curl -fsSL -o "${archive}" "${GO_DOWNLOAD_BASE}/${go_file}"
  local actual_sha=""
  actual_sha="$(sha256sum "${archive}" | awk '{print $1}')"
  [[ "${actual_sha}" == "${go_sha}" ]] || die "Go archive checksum mismatch"
  run_sudo rm -rf /usr/local/go
  run_sudo tar -C /usr/local -xzf "${archive}"
  rm -f "${metadata}" "${archive}"
}

ensure_go() {
  local required=""
  local installed=""
  required="$(required_go_version)"
  export PATH="/usr/local/go/bin:${PATH}"
  if installed="$(installed_go_version)" && semver_ge "${installed}" "${required}"; then
    log "Go ${installed} is already available."
    return
  fi
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Go is missing or older than ${required}; would install it from official Go downloads."
    install_go_from_official_archive "${required}"
    return
  fi
  log "Go is missing or older than ${required}; installing from official Go downloads."
  install_go_from_official_archive "${required}"
  export PATH="/usr/local/go/bin:${PATH}"
  command_exists go || die "Go install completed but go is not on PATH"
}

run_as_install_user() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    printf '[dry-run] '
    if [[ "${INSTALL_USER}" != "$(id -un)" || "${EUID}" -eq 0 ]]; then
      printf 'sudo -H -u %q ' "${INSTALL_USER}"
    fi
    printf 'env HOME=%q PATH=%q ' "${INSTALL_HOME}" "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:${PATH}"
    printf '%q ' "$@"
    printf '\n'
    return 0
  fi
  if [[ "${EUID}" -ne 0 && "${INSTALL_USER}" == "$(id -un)" ]]; then
    env HOME="${INSTALL_HOME}" PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:${PATH}" "$@"
  else
    "${SUDO:-sudo}" -H -u "${INSTALL_USER}" env HOME="${INSTALL_HOME}" PATH="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:${PATH}" "$@"
  fi
}

ensure_codex_cli() {
  if command_exists codex; then
    log "Codex CLI is already available: $(codex --version 2>/dev/null || printf 'version unknown')"
  else
    if [[ "${DRY_RUN}" -eq 1 ]]; then
      log "Would install Codex CLI with npm (${CODEX_NPM_PACKAGE})."
      return
    fi
    log "Installing Codex CLI with npm (${CODEX_NPM_PACKAGE})."
    run_sudo npm i -g "${CODEX_NPM_PACKAGE}"
  fi
  command_exists codex || die "Codex CLI install finished, but 'codex' is not on PATH"
}

ensure_claude_code_cli() {
	if command_exists claude; then
		log "Claude Code CLI is already available: $(claude --version 2>/dev/null || printf 'version unknown')"
	else
		if [[ "${DRY_RUN}" -eq 1 ]]; then
			log "Would install Claude Code CLI with npm (${CLAUDE_CODE_NPM_PACKAGE})."
			return
		fi
		log "Installing Claude Code CLI with npm (${CLAUDE_CODE_NPM_PACKAGE})."
		run_sudo npm i -g "${CLAUDE_CODE_NPM_PACKAGE}"
	fi
	command_exists claude || die "Claude Code CLI install finished, but 'claude' is not on PATH"
}

codex_auth_file() {
  if [[ -n "${CODEX_AUTH_FILE:-}" ]]; then
    printf '%s' "${CODEX_AUTH_FILE}"
  else
    printf '%s/.codex/auth.json' "${INSTALL_HOME}"
  fi
}

codex_auth_valid() {
  local auth_file="$1"
  [[ -s "${auth_file}" ]] || return 1
  python3 - "${auth_file}" <<'PY'
import json
import sys

path = sys.argv[1]
try:
    with open(path, "r", encoding="utf-8") as fh:
        data = json.load(fh)
except Exception:
    sys.exit(1)

if not isinstance(data, dict):
    sys.exit(1)

tokens = data.get("tokens")
if isinstance(tokens, dict) and any(tokens.get(k) for k in ("access_token", "refresh_token", "id_token")):
    sys.exit(0)

if any(data.get(k) for k in ("access_token", "refresh_token", "id_token", "OPENAI_API_KEY", "api_key")):
    sys.exit(0)

if data.get("accounts") or data.get("auth_mode"):
    sys.exit(0)

sys.exit(1)
PY
}

run_codex_login() {
  if codex login --help >/tmp/connect-ai-proxy-codex-login-help.txt 2>&1; then
    if grep -qi -- '--device-auth' /tmp/connect-ai-proxy-codex-login-help.txt; then
      run_as_install_user codex login --device-auth
    else
      run_as_install_user codex login
    fi
  else
    warn "Codex login command was not detected; running 'codex' so it can prompt for sign-in."
    run_as_install_user codex
  fi
  rm -f /tmp/connect-ai-proxy-codex-login-help.txt
}

ensure_codex_auth() {
  local auth_file=""
  auth_file="$(codex_auth_file)"
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would verify Codex auth at ${auth_file} and pause for login if needed."
    return
  fi
  if codex_auth_valid "${auth_file}"; then
    log "Codex auth is available at ${auth_file}."
    return
  fi
  [[ -t 0 ]] || die "Codex auth is missing at ${auth_file}, and this session is non-interactive. Run 'codex' as ${INSTALL_USER} first."
  warn "Codex auth is missing or invalid at ${auth_file}."
  log "Starting Codex login as ${INSTALL_USER}. Complete the browser/device sign-in, then return here."
  run_codex_login
  until codex_auth_valid "${auth_file}"; do
    local answer=""
    read -r -p "Press Enter after Codex login is complete, type 'retry' to run login again, or 'quit' to abort: " answer
    case "${answer,,}" in
      retry) run_codex_login ;;
      quit|q) die "Codex login was not completed" ;;
    esac
  done
  log "Codex auth verified."
}

# claude_logged_in reports whether the Claude Code CLI has working auth (a
# subscription login or an injected OAuth token). `claude auth status` exits 0
# when logged in, 1 otherwise.
claude_logged_in() {
  run_as_install_user claude auth status >/dev/null 2>&1
}

# ensure_claude_auth makes sure the Claude Code CLI can authenticate. If a token
# is already present (env or prior login) it returns. Otherwise it runs
# `claude setup-token` as the install user — which prints a long-lived OAuth
# token (requires a Claude subscription) — captures it, and stores it in
# CLAUDE_OAUTH_TOKEN_CAPTURED for configure_env_file to persist.
CLAUDE_OAUTH_TOKEN_CAPTURED=""
ensure_claude_auth() {
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would verify Claude Code auth and run 'claude setup-token' if needed."
    return
  fi
  if [[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]] || claude_logged_in; then
    log "Claude Code auth is available."
    return
  fi
  if [[ ! -t 0 ]]; then
    warn "Claude Code is not authenticated and this session is non-interactive."
    warn "Run 'claude setup-token' as ${INSTALL_USER} and set PROXY_CLAUDE_OAUTH_TOKEN in ${INSTALL_DIR}/.env."
    return
  fi
  log "Generating a long-lived Claude Code token (requires a Claude subscription)."
  log "Complete the browser/device sign-in if prompted; the token is printed below."
  local token=""
  token="$(run_as_install_user claude setup-token 2>/dev/null | grep -Eo 'sk-ant-oat[0-9A-Za-z_-]+' | tail -n 1 || true)"
  if [[ -n "${token}" ]]; then
    CLAUDE_OAUTH_TOKEN_CAPTURED="${token}"
    log "Captured a Claude Code OAuth token; it will be written to .env."
  else
    warn "Could not capture a token from 'claude setup-token'."
    warn "Set PROXY_CLAUDE_OAUTH_TOKEN in ${INSTALL_DIR}/.env manually, or run 'claude auth login' as ${INSTALL_USER}."
  fi
}

resolve_install_choices() {
  validate_port "${PROXY_PORT}" "proxy port"
  validate_port "${HTTP_PORT}" "HTTP port"
  validate_port "${HTTPS_PORT}" "HTTPS port"

  if [[ "${INSTALL_KIND}" == "ask" ]]; then
    if prompt_yes_no "Is this a server installation?" "no"; then
      INSTALL_KIND="server"
    else
      INSTALL_KIND="local"
    fi
  fi

  if [[ "${BROWSER_TOOLS}" == "ask" ]]; then
    local browser_default="yes"
    if [[ "${INSTALL_KIND}" == "server" ]]; then
      browser_default="no"
    fi
    if prompt_yes_no "Enable Chrome/Antigravity browser tools on this machine?" "${browser_default}"; then
      BROWSER_TOOLS="yes"
    else
      BROWSER_TOOLS="no"
    fi
  fi

  if [[ "${CLAUDE_CODE_CHOICE}" == "ask" ]]; then
    # Default to "yes" only if Claude Code is already installed, so a re-run /
    # non-interactive update preserves the prior choice and a fresh box opts out.
    local claude_default="no"
    if command_exists claude; then
      claude_default="yes"
    fi
    if [[ -t 0 ]]; then
      if prompt_yes_no "Install Claude Code CLI and use a Claude subscription as a 'claude' upstream?" "${claude_default}"; then
        CLAUDE_CODE_CHOICE="yes"
      else
        CLAUDE_CODE_CHOICE="no"
      fi
    else
      CLAUDE_CODE_CHOICE="${claude_default}"
    fi
  fi

  if [[ "${UPSTREAM_CHOICE}" == "ask" ]]; then
    UPSTREAM_CHOICE="codex"
  fi

  if [[ "${HTTPS_MODE}" == "ask" ]]; then
    if prompt_yes_no "Do you need a static domain with HTTPS for this installation?" "no"; then
      HTTPS_MODE="yes"
    else
      HTTPS_MODE="no"
    fi
  fi

  if [[ "${HTTPS_MODE}" == "yes" ]]; then
    DOMAIN="$(prompt_value "Domain name for HTTPS" "${DOMAIN}" "required")"
    EMAIL="$(prompt_value "Email address for HTTPS certificate registration" "${EMAIL}" "required")"
    PROXY_PORT="$(prompt_value "Internal proxy port" "${PROXY_PORT}" "required")"
    HTTP_PORT="$(prompt_value "Public HTTP redirect/ACME port" "${HTTP_PORT}" "required")"
    HTTPS_PORT="$(prompt_value "Public HTTPS port" "${HTTPS_PORT}" "required")"
    validate_port "${PROXY_PORT}" "proxy port"
    validate_port "${HTTP_PORT}" "HTTP port"
    validate_port "${HTTPS_PORT}" "HTTPS port"
    PROXY_HOST="127.0.0.1"
    if [[ -z "${PROXY_PUBLIC_URL}" ]]; then
      PROXY_PUBLIC_URL="$(public_url_for_port "https" "${DOMAIN}" "${HTTPS_PORT}")"
    fi
    if [[ "${DNS_CONFIRMED}" -ne 1 ]] && ! prompt_yes_no "Have this domain's DNS records already been pointed to this server?" "no"; then
      die "DNS must point to this server before Caddy can issue a trusted HTTPS certificate."
    fi
  elif [[ "${INSTALL_KIND}" == "server" ]]; then
    if [[ "${EXPOSE_HTTP}" == "ask" ]]; then
      if prompt_yes_no "Expose unencrypted HTTP publicly without HTTPS? This is not recommended." "no"; then
        EXPOSE_HTTP="yes"
      else
        EXPOSE_HTTP="no"
      fi
    fi
    if [[ "${EXPOSE_HTTP}" == "yes" ]]; then
      PROXY_HOST="0.0.0.0"
      warn "Public unencrypted HTTP is enabled. Use HTTPS whenever possible."
      if [[ -n "${DOMAIN}" && -z "${PROXY_PUBLIC_URL}" ]]; then
        PROXY_PUBLIC_URL="$(public_url_for_port "http" "${DOMAIN}" "${PROXY_PORT}")"
      fi
    else
      PROXY_HOST="127.0.0.1"
    fi
  else
    PROXY_HOST="127.0.0.1"
  fi
}

find_chrome_command() {
  local name=""
  for name in google-chrome google-chrome-stable chromium chromium-browser; do
    if command_exists "${name}"; then
      command -v "${name}"
      return 0
    fi
  done
  return 1
}

install_browser_dependencies() {
  if [[ "${BROWSER_TOOLS}" != "yes" ]]; then
    log "Browser tools are disabled; skipping Chrome/Chromium installation and MCP browser setup."
    return
  fi
  if find_chrome_command >/dev/null 2>&1; then
    log "Chrome/Chromium is already available: $(find_chrome_command)"
  else
    local candidates=()
    case "${PKG_MANAGER}" in
      apt) candidates=(chromium chromium-browser google-chrome-stable) ;;
      dnf|yum) candidates=(chromium google-chrome-stable) ;;
      zypper) candidates=(chromium google-chrome-stable) ;;
      pacman) candidates=(chromium google-chrome) ;;
      apk) candidates=(chromium) ;;
    esac
    local package=""
    for package in "${candidates[@]}"; do
      if package_available "${package}"; then
        log "Installing browser package '${package}'."
        package_install "${package}"
        break
      fi
    done
    if ! find_chrome_command >/dev/null 2>&1; then
      warn "Chrome/Chromium was not found in configured package repositories. Install Chrome or Chromium manually before using browser tools."
    fi
  fi
  warn "Install the Antigravity Chrome extension in the browser profile before using browser MCP tools. Extension ID: eeijfnjmjelapkebgockoeaadonbchdd"
}

assert_safe_install_dir() {
  case "${INSTALL_DIR}" in
    ""|"/"|"/opt"|"/usr"|"/usr/local"|"/home") die "unsafe install directory: ${INSTALL_DIR}" ;;
  esac
}

build_proxy() {
  log "Building ${APP_NAME}."
  run mkdir -p "${REPO_DIR}/bin"
  cd "${REPO_DIR}" || die "failed to enter ${REPO_DIR}"
  run env PATH="/usr/local/go/bin:${PATH}" go mod download
  run env PATH="/usr/local/go/bin:${PATH}" go build -o "${REPO_DIR}/bin/${APP_NAME}" .
  # agyj is a separate nested module (the "agy" upstream wrapper) — `go build .`
  # above skips it, so build it explicitly next to the proxy binary.
  if [[ -f "${REPO_DIR}/agyj/go.mod" ]]; then
    log "Building agyj (agy upstream wrapper)."
    ( cd "${REPO_DIR}/agyj" \
      && run env PATH="/usr/local/go/bin:${PATH}" go mod download \
      && run env PATH="/usr/local/go/bin:${PATH}" go build -o "${REPO_DIR}/bin/agyj" . ) \
      || log "WARNING: agyj build failed; the Agy upstream will be unavailable."
  fi
}

copy_proxy_files() {
  assert_safe_install_dir
  log "Installing files to ${INSTALL_DIR}."
  run_sudo mkdir -p "${INSTALL_DIR}"
  run_sudo chown -R "${INSTALL_USER}:${INSTALL_GROUP}" "${INSTALL_DIR}"
  run mkdir -p "${INSTALL_DIR}"
  local binary_tmp="${INSTALL_DIR}/.${APP_NAME}.new.$$"
  run cp "${REPO_DIR}/bin/${APP_NAME}" "${binary_tmp}"
  run chmod 0755 "${binary_tmp}"
  run mv -f "${binary_tmp}" "${INSTALL_DIR}/${APP_NAME}"
  # Ship agyj alongside the proxy binary; the proxy resolves it as a sibling of
  # its own executable (PROXY_AGY_BIN overrides). Atomic swap to avoid a torn file.
  if [[ -f "${REPO_DIR}/bin/agyj" ]]; then
    local agyj_tmp="${INSTALL_DIR}/.agyj.new.$$"
    run cp "${REPO_DIR}/bin/agyj" "${agyj_tmp}"
    run chmod 0755 "${agyj_tmp}"
    run mv -f "${agyj_tmp}" "${INSTALL_DIR}/agyj"
  fi
  run rm -rf "${INSTALL_DIR}/ui"
  run cp -R "${REPO_DIR}/ui" "${INSTALL_DIR}/ui"
  for helper in VERSION update-linux.sh reinstall-linux.sh uninstall-linux.sh install-linux.sh; do
    if [[ -f "${REPO_DIR}/${helper}" ]]; then
      run cp "${REPO_DIR}/${helper}" "${INSTALL_DIR}/${helper}"
    fi
  done
  if [[ -f "${REPO_DIR}/.env.example" ]]; then
    run cp "${REPO_DIR}/.env.example" "${INSTALL_DIR}/.env.example"
  fi
  run_sudo chown -R "${INSTALL_USER}:${INSTALL_GROUP}" "${INSTALL_DIR}"
  run_sudo ln -sfn "${INSTALL_DIR}/${APP_NAME}" "/usr/local/bin/${APP_NAME}"
}

random_secret() {
  (set +o pipefail; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 48)
}

get_env_value() {
  local file="$1"
  local key="$2"
  [[ -f "${file}" ]] || return 1
  local line=""
  line="$(grep -E "^${key}=" "${file}" | tail -n 1 || true)"
  [[ -n "${line}" ]] || return 1
  printf '%s' "${line#*=}"
}

set_env_value() {
  local file="$1"
  local key="$2"
  local value="$3"
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would set ${key} in ${file}."
    return 0
  fi
  touch "${file}"
  chmod 0600 "${file}"
  local tmp=""
  tmp="$(mktemp)"
  awk -v key="${key}" -v value="${value}" '
    BEGIN { done = 0 }
    $0 ~ "^" key "=" {
      print key "=" value
      done = 1
      next
    }
    { print }
    END {
      if (!done) print key "=" value
    }
  ' "${file}" > "${tmp}"
  mv "${tmp}" "${file}"
  chmod 0600 "${file}"
}

configure_env_file() {
  local env_file="${INSTALL_DIR}/.env"
  if [[ ! -f "${env_file}" ]]; then
    if [[ -f "${INSTALL_DIR}/.env.example" ]]; then
      run cp "${INSTALL_DIR}/.env.example" "${env_file}"
    else
      run touch "${env_file}"
    fi
  fi
  set_env_value "${env_file}" "CODEX_AUTH_FILE" "$(codex_auth_file)"
  set_env_value "${env_file}" "PROXY_HOST" "${PROXY_HOST}"
  set_env_value "${env_file}" "PROXY_PORT" "${PROXY_PORT}"
  set_env_value "${env_file}" "PROXY_PUBLIC_URL" "${PROXY_PUBLIC_URL}"
  set_env_value "${env_file}" "PROXY_UPDATE_REPO_DIR" "${REPO_DIR}"
  if [[ "${BROWSER_TOOLS}" == "yes" ]]; then
    set_env_value "${env_file}" "ANTIGRAVITY_BROWSER_ENABLED" "1"
  else
    set_env_value "${env_file}" "ANTIGRAVITY_BROWSER_ENABLED" "0"
    set_env_value "${env_file}" "ANTIGRAVITY_BROWSER_PRELAUNCH_WITH_PROXY" "0"
  fi
  local current_key=""
  current_key="$(get_env_value "${env_file}" "PROXY_API_KEY" || true)"
  if [[ -z "${current_key}" || "${current_key}" == "sk-local-proxy-token" ]]; then
    set_env_value "${env_file}" "PROXY_API_KEY" "sk-local-$(random_secret)"
  fi
  local session_secret=""
  session_secret="$(get_env_value "${env_file}" "ADMIN_SESSION_SECRET" || true)"
  if [[ -z "${session_secret}" ]]; then
    set_env_value "${env_file}" "ADMIN_SESSION_SECRET" "$(random_secret)"
  fi
  if [[ "${UPSTREAM_CHOICE}" != "ask" && -n "${UPSTREAM_CHOICE}" ]]; then
    set_env_value "${env_file}" "UPSTREAM" "${UPSTREAM_CHOICE}"
  fi
  if [[ "${CLAUDE_CODE_CHOICE}" == "yes" ]]; then
    set_env_value "${env_file}" "PROXY_CLAUDE_ENABLED" "1"
    local claude_bin=""
    claude_bin="$(command -v claude 2>/dev/null || true)"
    [[ -n "${claude_bin}" ]] && set_env_value "${env_file}" "PROXY_CLAUDE_BIN" "${claude_bin}"
    if [[ -n "${CLAUDE_OAUTH_TOKEN_CAPTURED}" ]]; then
      set_env_value "${env_file}" "PROXY_CLAUDE_OAUTH_TOKEN" "${CLAUDE_OAUTH_TOKEN_CAPTURED}"
    fi
    # Neutral working dir so a stray project CLAUDE.md never leaks into prompts.
    set_env_value "${env_file}" "PROXY_CLAUDE_WORKDIR" "${INSTALL_DIR}/.claude-workdir"
    run install -d -m 0700 -o "${INSTALL_USER}" -g "${INSTALL_GROUP}" "${INSTALL_DIR}/.claude-workdir" 2>/dev/null || run mkdir -p "${INSTALL_DIR}/.claude-workdir"
  elif [[ "${CLAUDE_CODE_CHOICE}" == "no" ]]; then
    set_env_value "${env_file}" "PROXY_CLAUDE_ENABLED" "0"
  fi
  if [[ -n "${GEMINI_API_KEY_CHOICE}" ]]; then
    set_env_value "${env_file}" "GEMINI_API_KEY" "${GEMINI_API_KEY_CHOICE}"
    set_env_value "${env_file}" "ANTIGRAVITY_API_KEY" "${GEMINI_API_KEY_CHOICE}"
  fi
  run_sudo chown "${INSTALL_USER}:${INSTALL_GROUP}" "${env_file}"
}

ensure_systemd() {
  command_exists systemctl || die "systemd/systemctl is required for ${SERVICE_NAME}. This installer cannot register startup on non-systemd Linux."
}

write_systemd_service() {
  ensure_systemd
  local service_path="/etc/systemd/system/${SERVICE_NAME}"
  local path_value="/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"
  log "Writing systemd service ${service_path}."
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would write ${service_path} for user ${INSTALL_USER}."
  else
    local tmp=""
    tmp="$(mktemp)"
    cat > "${tmp}" <<SERVICE
[Unit]
# Never give up restarting: StartLimitIntervalSec=0 disables systemd's start-rate
# limiter, so a crash loop or a botched self-update can never leave the proxy
# permanently dead waiting for a human to restart it by hand.
StartLimitIntervalSec=0
Description=Connect AI Proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${INSTALL_USER}
WorkingDirectory=${INSTALL_DIR}
Environment=HOME=${INSTALL_HOME}
Environment=PATH=${path_value}
ExecStartPre=/usr/local/bin/${APP_NAME} sync apply
ExecStart=/usr/local/bin/${APP_NAME} serve
ExecReload=/usr/local/bin/${APP_NAME} sync apply
ExecStopPost=/usr/local/bin/${APP_NAME} sync restore
# Restart=always (not on-failure) so a clean exit/SIGTERM also triggers recovery.
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
SERVICE
    run_sudo mv "${tmp}" "${service_path}"
    run_sudo chmod 0644 "${service_path}"
  fi
  run_sudo systemctl daemon-reload
}

install_caddy_official_apt() {
  log "Installing Caddy from the official Caddy apt repository."
  package_install debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  run_sudo rm -f /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  sudo_shell "curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg"
  sudo_shell "curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list"
  run_sudo chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  run_sudo chmod o+r /etc/apt/sources.list.d/caddy-stable.list
  package_install caddy
}

install_caddy_official_dnf() {
  log "Installing Caddy from the official Caddy COPR repository."
  if ! package_install dnf5-plugins; then
    package_install dnf-plugins-core
  fi
  run_sudo dnf copr enable -y @caddy/caddy
  package_install caddy
}

ensure_caddy() {
  if [[ "${HTTPS_MODE}" != "yes" ]]; then
    return
  fi
  if command_exists caddy; then
    log "Caddy is already available."
    return
  fi
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would install Caddy for HTTPS reverse proxy support."
    return
  fi
  if package_available caddy; then
    package_install caddy
  else
    case "${PKG_MANAGER}" in
      apt) install_caddy_official_apt ;;
      dnf) install_caddy_official_dnf ;;
      *) die "Caddy is not available from this package manager. Install Caddy first, then rerun with --https." ;;
    esac
  fi
  command_exists caddy || die "Caddy install finished, but caddy is not on PATH"
}

configure_caddy() {
  if [[ "${HTTPS_MODE}" != "yes" ]]; then
    return
  fi
  ensure_systemd
  local caddyfile="/etc/caddy/Caddyfile"
  log "Configuring Caddy HTTPS reverse proxy for ${DOMAIN}."
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "Would write ${caddyfile}, validate it, and reload caddy."
  else
    local tmp=""
    tmp="$(mktemp)"
    cat > "${tmp}" <<CADDY
{
	email ${EMAIL}
	http_port ${HTTP_PORT}
	https_port ${HTTPS_PORT}
}

${DOMAIN} {
	encode zstd gzip
	reverse_proxy 127.0.0.1:${PROXY_PORT} {
		# Ride through brief upstream gaps (proxy restart/deploy) instead of a
		# hard 502 — when Connect runs without a fallback a momentary gap must
		# not reach customers. Verified: a full proxy restart yields 0x 502.
		lb_try_duration 15s
		lb_try_interval 250ms
	}
}
CADDY
    run_sudo mkdir -p /etc/caddy
    run_sudo mv "${tmp}" "${caddyfile}"
    run_sudo chmod 0644 "${caddyfile}"
    run_sudo caddy validate --config "${caddyfile}"
  fi
  if [[ "${NO_ENABLE}" -eq 0 ]]; then
    run_sudo systemctl enable caddy
  fi
  run_sudo systemctl reload caddy || run_sudo systemctl restart caddy
}

start_or_enable_service() {
  if [[ "${NO_ENABLE}" -eq 0 ]]; then
    run_sudo systemctl enable "${SERVICE_NAME}"
  fi
  if [[ "${NO_START}" -eq 0 ]]; then
    run_sudo systemctl restart "${SERVICE_NAME}"
    run_sudo systemctl --no-pager --full status "${SERVICE_NAME}"
  else
    log "Skipping service start because --no-start was provided."
  fi
}

manage_service() {
  require_sudo
  ensure_systemd
  case "${ACTION}" in
    start) run_sudo systemctl start "${SERVICE_NAME}" ;;
    stop) run_sudo systemctl stop "${SERVICE_NAME}" ;;
    reload) run_sudo systemctl reload "${SERVICE_NAME}" ;;
    restart) run_sudo systemctl restart "${SERVICE_NAME}" ;;
    status) run_sudo systemctl --no-pager --full status "${SERVICE_NAME}" ;;
    enable) run_sudo systemctl enable --now "${SERVICE_NAME}" ;;
    disable) run_sudo systemctl disable --now "${SERVICE_NAME}" ;;
    *) die "unsupported service action: ${ACTION}" ;;
  esac
}

uninstall_service() {
  require_sudo
  ensure_systemd
  run_sudo systemctl disable --now "${SERVICE_NAME}" || true
  run_sudo rm -f "/etc/systemd/system/${SERVICE_NAME}"
  run_sudo systemctl daemon-reload
  run_sudo rm -f "/usr/local/bin/${APP_NAME}"
  if prompt_yes_no "Remove ${INSTALL_DIR} as well?" "no"; then
    assert_safe_install_dir
    run_sudo rm -rf "${INSTALL_DIR}"
  else
    log "Kept ${INSTALL_DIR}."
  fi
}

install_all() {
  [[ "$(uname -s)" == "Linux" ]] || die "this installer is only for Linux"
  require_sudo
  detect_install_user
  detect_os_and_package_manager
	resolve_install_choices
	install_base_dependencies
	install_media_dependencies
	ensure_node_runtime
	if [[ "${CLAUDE_CODE_CHOICE}" == "yes" ]]; then
		ensure_claude_code_cli
		ensure_claude_auth
	fi
	ensure_go
	if [[ "${UPSTREAM_CHOICE}" != "openai" ]]; then
		ensure_codex_cli
		ensure_codex_auth
	fi
  install_browser_dependencies
  build_proxy
  copy_proxy_files
  configure_env_file
  write_systemd_service
  ensure_caddy
  configure_caddy
  start_or_enable_service
  log "Install complete."
  log "Local health check: http://127.0.0.1:${PROXY_PORT}/health"
  if [[ "${HTTPS_MODE}" == "yes" ]]; then
    log "HTTPS endpoint: ${PROXY_PUBLIC_URL}/health"
  fi
}

main() {
  parse_args "$@"
  case "${ACTION}" in
    install) install_all ;;
    start|stop|reload|restart|status|enable|disable) manage_service ;;
    uninstall) uninstall_service ;;
    *) die "unsupported action: ${ACTION}" ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
