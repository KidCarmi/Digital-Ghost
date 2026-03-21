#!/usr/bin/env bash
# =============================================================================
# Digital Ghost — Prerequisites Setup Script
# =============================================================================
# Checks for every prerequisite, installs if missing, upgrades if outdated.
#
# Supported platforms:
#   Linux  — Debian/Ubuntu, Fedora/RHEL, Arch, openSUSE
#   macOS  — 12.3+ (Monterey or later)
#   Windows — use scripts/setup.ps1 instead (PowerShell)
#
# Usage:
#   chmod +x scripts/setup.sh
#   ./scripts/setup.sh              # Check + install/upgrade everything
#   ./scripts/setup.sh --check-only # Check only, no installs
#   ./scripts/setup.sh --no-model   # Skip Ollama model pull (large download)
# =============================================================================

set -euo pipefail

# ── Constants ──────────────────────────────────────────────────────────────────
readonly REQUIRED_GO_MAJOR=1
readonly REQUIRED_GO_MINOR=22
readonly REQUIRED_GO_VERSION="${REQUIRED_GO_MAJOR}.${REQUIRED_GO_MINOR}"

readonly OLLAMA_MIN_VERSION="0.1.30"
readonly DEFAULT_OLLAMA_MODEL="llava:7b"

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ── Flags ──────────────────────────────────────────────────────────────────────
CHECK_ONLY=false
SKIP_MODEL=false
for arg in "$@"; do
  case "$arg" in
    --check-only) CHECK_ONLY=true ;;
    --no-model)   SKIP_MODEL=true ;;
    --help|-h)
      echo "Usage: $0 [--check-only] [--no-model]"
      echo "  --check-only  Print status of each prerequisite; do not install anything"
      echo "  --no-model    Skip pulling the Ollama vision model (large download)"
      exit 0
      ;;
  esac
done

# ── Colour output ─────────────────────────────────────────────────────────────
if [ -t 1 ] && command -v tput &>/dev/null && tput colors &>/dev/null && [ "$(tput colors)" -ge 8 ]; then
  RED=$(tput setaf 1); GREEN=$(tput setaf 2); YELLOW=$(tput setaf 3)
  CYAN=$(tput setaf 6); BOLD=$(tput bold); RESET=$(tput sgr0)
else
  RED=""; GREEN=""; YELLOW=""; CYAN=""; BOLD=""; RESET=""
fi

# ── Logging helpers ────────────────────────────────────────────────────────────
info()    { echo "${CYAN}[INFO]${RESET}  $*"; }
ok()      { echo "${GREEN}[ OK ]${RESET}  $*"; }
warn()    { echo "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo "${RED}[ERR ]${RESET}  $*" >&2; }
section() { echo; echo "${BOLD}━━━  $*  ━━━${RESET}"; }
die()     { error "$*"; exit 1; }

# ── Platform detection ─────────────────────────────────────────────────────────
detect_platform() {
  OS="$(uname -s)"
  ARCH="$(uname -m)"

  case "$OS" in
    MINGW*|MSYS*|CYGWIN*|Windows_NT)
      die "Windows detected. Please use the PowerShell setup script instead:
  Right-click scripts\\setup.ps1 > 'Run with PowerShell'
  OR: powershell -ExecutionPolicy Bypass -File scripts\\setup.ps1"
      ;;
    Linux)
      PLATFORM="linux"
      # Detect distro family.
      if [ -f /etc/os-release ]; then
        # shellcheck disable=SC1091
        source /etc/os-release
        case "${ID_LIKE:-$ID}" in
          *debian*|*ubuntu*) DISTRO_FAMILY="debian" ;;
          *fedora*|*rhel*|*centos*) DISTRO_FAMILY="fedora" ;;
          *arch*|*manjaro*) DISTRO_FAMILY="arch" ;;
          *suse*|*opensuse*) DISTRO_FAMILY="suse" ;;
          *) DISTRO_FAMILY="unknown" ;;
        esac
      else
        DISTRO_FAMILY="unknown"
      fi
      ;;
    Darwin)
      PLATFORM="macos"
      DISTRO_FAMILY="macos"
      ;;
    *)
      die "Unsupported platform: $OS. Digital Ghost supports Linux and macOS."
      ;;
  esac

  case "$ARCH" in
    x86_64|amd64) ARCH_NORMALIZED="amd64" ;;
    aarch64|arm64) ARCH_NORMALIZED="arm64" ;;
    *) die "Unsupported architecture: $ARCH" ;;
  esac

  info "Platform: ${OS} / ${ARCH} (distro family: ${DISTRO_FAMILY})"
}

# ── Package manager helpers ────────────────────────────────────────────────────
pkg_install() {
  # Usage: pkg_install <friendly-name> <pkg1> [pkg2 ...]
  local name="$1"; shift
  if $CHECK_ONLY; then
    warn "MISSING: $name — would install: $*"
    return
  fi
  info "Installing $name..."
  case "$DISTRO_FAMILY" in
    debian) sudo apt-get install -y "$@" ;;
    fedora) sudo dnf install -y "$@" ;;
    arch)   sudo pacman -S --noconfirm "$@" ;;
    suse)   sudo zypper install -y "$@" ;;
    macos)  brew install "$@" ;;
    *)      warn "Unknown distro; cannot auto-install $name. Install manually: $*" ;;
  esac
}

pkg_update_index() {
  $CHECK_ONLY && return
  case "$DISTRO_FAMILY" in
    debian) sudo apt-get update -qq ;;
    fedora) : ;; # dnf auto-refreshes
    arch)   sudo pacman -Sy --noconfirm ;;
    suse)   sudo zypper refresh ;;
    macos)  brew update ;;
  esac
}

# ── Version comparison ─────────────────────────────────────────────────────────
# Returns 0 if $1 >= $2 (both are "MAJOR.MINOR[.PATCH]" strings)
version_gte() {
  python3 -c "
import sys
def parse(v): return tuple(int(x) for x in v.split('.')[:3])
a, b = parse('$1'), parse('$2')
sys.exit(0 if a >= b else 1)
" 2>/dev/null || \
  awk -v a="$1" -v b="$2" 'BEGIN {
    split(a,av,"."); split(b,bv,".")
    for(i=1;i<=3;i++){
      ai=av[i]+0; bi=bv[i]+0
      if(ai>bi) exit 0
      if(ai<bi) exit 1
    }
    exit 0
  }'
}

# ── Prerequisite checks ────────────────────────────────────────────────────────

check_root_guard() {
  section "Root Guard"
  if [ "$(id -u)" -eq 0 ]; then
    die "Do not run this script as root. It will use sudo where needed."
  fi
  ok "Running as non-root user ($(whoami))"
}

check_curl() {
  section "curl"
  if command -v curl &>/dev/null; then
    ok "curl: $(curl --version | head -1)"
  else
    pkg_install "curl" curl
  fi
}

check_git() {
  section "git"
  if command -v git &>/dev/null; then
    ok "git: $(git --version)"
  else
    pkg_install "git" git
  fi
}

check_go() {
  section "Go ${REQUIRED_GO_VERSION}+"
  local installed_version=""

  if command -v go &>/dev/null; then
    installed_version="$(go version | grep -oP '\d+\.\d+(\.\d+)?' | head -1)"
    if version_gte "$installed_version" "${REQUIRED_GO_VERSION}"; then
      ok "Go ${installed_version} — satisfies >= ${REQUIRED_GO_VERSION}"
      return
    else
      warn "Go ${installed_version} is below required ${REQUIRED_GO_VERSION}"
    fi
  else
    warn "Go not found"
  fi

  if $CHECK_ONLY; then
    warn "Would install Go ${REQUIRED_GO_VERSION}"
    return
  fi

  info "Installing Go via official tarball..."
  install_go_official
}

install_go_official() {
  local go_version
  # Fetch the latest stable Go version >= REQUIRED_GO_VERSION.
  go_version=$(curl -fsSL "https://go.dev/VERSION?m=text" | head -1 | tr -d '[:space:]')
  go_version="${go_version#go}" # strip leading "go"

  local tarball="go${go_version}.linux-${ARCH_NORMALIZED}.tar.gz"
  local url="https://go.dev/dl/${tarball}"
  local tmp_dir
  tmp_dir="$(mktemp -d)"

  info "Downloading ${url}..."
  curl -fsSL "$url" -o "${tmp_dir}/${tarball}"

  info "Installing to /usr/local/go ..."
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf "${tmp_dir}/${tarball}"
  rm -rf "$tmp_dir"

  # Ensure /usr/local/go/bin is in PATH for this session.
  export PATH="/usr/local/go/bin:${PATH}"

  # Add to shell profile if not already there.
  local profile_line='export PATH="/usr/local/go/bin:$PATH"'
  for f in "$HOME/.bashrc" "$HOME/.zshrc" "$HOME/.profile"; do
    if [ -f "$f" ] && ! grep -q 'usr/local/go/bin' "$f"; then
      echo "$profile_line" >> "$f"
      info "Added Go to PATH in $f"
    fi
  done

  ok "Go $(go version) installed"
}

check_ollama() {
  section "Ollama"
  if command -v ollama &>/dev/null; then
    local ver
    ver="$(ollama --version 2>/dev/null | grep -oP '\d+\.\d+\.\d+' | head -1 || echo "unknown")"
    if [ "$ver" = "unknown" ] || version_gte "$ver" "$OLLAMA_MIN_VERSION"; then
      ok "ollama ${ver}"
    else
      warn "ollama ${ver} is below recommended ${OLLAMA_MIN_VERSION}"
      if ! $CHECK_ONLY; then
        info "Upgrading Ollama..."
        install_ollama
      fi
    fi
  else
    warn "Ollama not found"
    if ! $CHECK_ONLY; then
      install_ollama
    else
      warn "Would install Ollama from https://ollama.com/install.sh"
    fi
  fi
}

install_ollama() {
  info "Installing Ollama (official installer)..."
  curl -fsSL https://ollama.com/install.sh | sh
  ok "Ollama installed: $(ollama --version 2>/dev/null || echo 'version unknown')"
}

check_ollama_running() {
  section "Ollama Service"
  if curl -sf "http://127.0.0.1:11434/api/tags" &>/dev/null; then
    ok "Ollama is running at http://127.0.0.1:11434"
    return
  fi
  warn "Ollama is not running"
  if $CHECK_ONLY; then
    warn "Would start Ollama service"
    return
  fi
  info "Starting Ollama service..."
  if command -v systemctl &>/dev/null && systemctl is-active --quiet ollama 2>/dev/null; then
    :
  elif command -v ollama &>/dev/null; then
    ollama serve &>/dev/null &
    disown
    sleep 2
    if curl -sf "http://127.0.0.1:11434/api/tags" &>/dev/null; then
      ok "Ollama service started"
    else
      warn "Ollama service may still be starting; check with: ollama serve"
    fi
  fi
}

check_ollama_model() {
  section "Ollama Vision Model (${DEFAULT_OLLAMA_MODEL})"
  if $SKIP_MODEL; then
    info "Skipping model check (--no-model)"
    return
  fi

  if ! command -v ollama &>/dev/null; then
    warn "Ollama not installed; skipping model check"
    return
  fi

  if ! curl -sf "http://127.0.0.1:11434/api/tags" &>/dev/null; then
    warn "Ollama not running; skipping model check"
    return
  fi

  local model_name="${DEFAULT_OLLAMA_MODEL%%:*}"
  if ollama list 2>/dev/null | grep -q "^${model_name}"; then
    ok "${DEFAULT_OLLAMA_MODEL} is available"
  else
    warn "${DEFAULT_OLLAMA_MODEL} not found locally"
    if $CHECK_ONLY; then
      warn "Would pull: ollama pull ${DEFAULT_OLLAMA_MODEL}"
      warn "Note: llava:7b is ~4.5 GB download"
      return
    fi
    info "Pulling ${DEFAULT_OLLAMA_MODEL} (~4.5 GB)..."
    ollama pull "${DEFAULT_OLLAMA_MODEL}"
    ok "${DEFAULT_OLLAMA_MODEL} pulled"
  fi
}

check_linux_x11_deps() {
  section "Linux X11 / Screen Capture Libraries"
  [ "$PLATFORM" != "linux" ] && return

  local missing=()

  # X11 base (for XShmGetImage)
  check_lib "libx11"       "libX11.so"   missing
  check_lib "libxext"      "libXext.so"  missing  # MIT-SHM extension
  check_lib "libxrandr"    "libXrandr.so" missing  # display enumeration

  # AT-SPI2 for accessibility (URL bar, focused input role)
  check_lib "libatspi"     "libatspi.so"  missing

  if [ ${#missing[@]} -gt 0 ]; then
    warn "Missing libraries: ${missing[*]}"
    if ! $CHECK_ONLY; then
      case "$DISTRO_FAMILY" in
        debian) pkg_install "X11/AT-SPI2 libraries" \
                  libx11-dev libxext-dev libxrandr-dev \
                  libatspi2.0-dev at-spi2-core ;;
        fedora) pkg_install "X11/AT-SPI2 libraries" \
                  libX11-devel libXext-devel libXrandr-devel \
                  at-spi2-core-devel ;;
        arch)   pkg_install "X11/AT-SPI2 libraries" \
                  libx11 libxext libxrandr at-spi2-core ;;
        suse)   pkg_install "X11/AT-SPI2 libraries" \
                  libX11-devel libXext-devel libXrandr-devel \
                  at-spi2-core-devel ;;
        *)      warn "Cannot auto-install on unknown distro. Install: ${missing[*]}" ;;
      esac
    fi
  else
    ok "All X11/AT-SPI2 libraries present"
  fi
}

check_lib() {
  # Usage: check_lib <friendly-name> <so-pattern> <missing-array-nameref>
  local name="$1" pattern="$2"
  local -n _missing_ref=$3
  if ldconfig -p 2>/dev/null | grep -q "$pattern" || \
     find /usr/lib /usr/local/lib /lib -name "${pattern}*" 2>/dev/null | grep -q .; then
    return 0
  fi
  _missing_ref+=("$name")
}

check_linux_wayland_deps() {
  section "Linux Wayland / PipeWire Libraries"
  [ "$PLATFORM" != "linux" ] && return

  # Check if we're even running on Wayland (optional — X11 is the primary path).
  if [ -z "${WAYLAND_DISPLAY:-}" ]; then
    info "WAYLAND_DISPLAY not set — Wayland capture optional (X11 path will be used)"
    return
  fi

  local missing=()

  # xdg-desktop-portal for ScreenCast
  if ! command -v xdg-desktop-portal &>/dev/null && \
     ! systemctl --user is-active --quiet xdg-desktop-portal 2>/dev/null; then
    missing+=("xdg-desktop-portal")
  fi

  # PipeWire
  if ! command -v pipewire &>/dev/null; then
    missing+=("pipewire")
  fi

  if [ ${#missing[@]} -gt 0 ]; then
    warn "Missing Wayland deps: ${missing[*]}"
    if ! $CHECK_ONLY; then
      case "$DISTRO_FAMILY" in
        debian) pkg_install "Wayland/PipeWire" xdg-desktop-portal pipewire libpipewire-0.3-dev ;;
        fedora) pkg_install "Wayland/PipeWire" xdg-desktop-portal pipewire pipewire-devel ;;
        arch)   pkg_install "Wayland/PipeWire" xdg-desktop-portal pipewire ;;
        *)      warn "Install manually: ${missing[*]}" ;;
      esac
    fi
  else
    ok "Wayland/PipeWire dependencies present"
  fi
}

check_linux_keychain_deps() {
  section "OS Keychain (libsecret)"
  [ "$PLATFORM" != "linux" ] && return

  local missing=()
  check_lib "libsecret" "libsecret" missing

  if [ ${#missing[@]} -gt 0 ]; then
    warn "libsecret not found — required for encryption key storage"
    if ! $CHECK_ONLY; then
      case "$DISTRO_FAMILY" in
        debian) pkg_install "libsecret" libsecret-1-dev gnome-keyring ;;
        fedora) pkg_install "libsecret" libsecret-devel gnome-keyring ;;
        arch)   pkg_install "libsecret" libsecret gnome-keyring ;;
        suse)   pkg_install "libsecret" libsecret-devel gnome-keyring ;;
        *)      warn "Install libsecret and a keyring daemon (gnome-keyring or kwallet)" ;;
      esac
    fi
  else
    ok "libsecret available"
    # Check that a keyring daemon is running.
    if ! pgrep -x gnome-keyring-daemon &>/dev/null && ! pgrep -x kwalletd5 &>/dev/null && ! pgrep -x kwalletd6 &>/dev/null; then
      warn "No keyring daemon detected (gnome-keyring-daemon / kwalletd)."
      warn "Digital Ghost requires a keyring daemon to store the encryption key."
      warn "Start one with: gnome-keyring-daemon --start --components=secrets"
    else
      ok "Keyring daemon is running"
    fi
  fi
}

check_macos_deps() {
  section "macOS Dependencies"
  [ "$PLATFORM" != "macos" ] && return

  # Homebrew
  if ! command -v brew &>/dev/null; then
    warn "Homebrew not found — required for macOS package management"
    if ! $CHECK_ONLY; then
      info "Installing Homebrew..."
      /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
    else
      warn "Install Homebrew from https://brew.sh"
    fi
  else
    ok "Homebrew: $(brew --version | head -1)"
  fi

  # macOS version check (ScreenCaptureKit requires 12.3+)
  local macos_version
  macos_version="$(sw_vers -productVersion)"
  if version_gte "$macos_version" "12.3"; then
    ok "macOS ${macos_version} — ScreenCaptureKit supported"
  else
    warn "macOS ${macos_version} is below 12.3 (Monterey). ScreenCaptureKit requires 12.3+."
    warn "Screen capture on older macOS will use the CGWindow API (deprecated)."
  fi

  # Check Screen Recording permission (TCC).
  # We can only check; granting it requires a user action in System Settings.
  info "Screen Recording permission must be granted in:"
  info "  System Settings > Privacy & Security > Screen Recording"
  info "  This is the same permission Zoom and OBS request."
}

check_python3() {
  # Python 3 is used for version comparison fallback in this script.
  section "Python 3 (optional — used in version comparison)"
  if command -v python3 &>/dev/null; then
    ok "python3: $(python3 --version)"
  else
    info "python3 not found — version comparisons will use awk fallback (fine)"
  fi
}

check_build_tools() {
  section "Build Tools (gcc / clang)"
  if [ "$PLATFORM" = "linux" ]; then
    if command -v gcc &>/dev/null || command -v cc &>/dev/null; then
      ok "C compiler: $(cc --version 2>/dev/null | head -1 || echo 'present')"
    else
      warn "No C compiler found — required for cgo dependencies (libsecret, X11)"
      if ! $CHECK_ONLY; then
        case "$DISTRO_FAMILY" in
          debian) pkg_install "build-essential" build-essential ;;
          fedora) pkg_install "gcc" gcc ;;
          arch)   pkg_install "base-devel" base-devel ;;
          suse)   pkg_install "gcc" gcc make ;;
        esac
      fi
    fi
  elif [ "$PLATFORM" = "macos" ]; then
    if xcode-select -p &>/dev/null; then
      ok "Xcode Command Line Tools present"
    else
      warn "Xcode Command Line Tools not found"
      if ! $CHECK_ONLY; then
        info "Installing Xcode Command Line Tools..."
        xcode-select --install || true
      fi
    fi
  fi
}

check_go_build() {
  section "Go Build Verification"
  if ! command -v go &>/dev/null; then
    warn "Go not available — skipping build check"
    return
  fi
  if $CHECK_ONLY; then
    info "Would run: go build ./..."
    return
  fi
  info "Running go build ./... in ${REPO_ROOT}"
  cd "$REPO_ROOT"
  if go build ./... 2>&1; then
    ok "go build ./... succeeded"
  else
    warn "go build ./... failed — see errors above"
    warn "This may be expected until all cgo dependencies are installed"
  fi
}

# ── Summary ────────────────────────────────────────────────────────────────────
print_summary() {
  section "Setup Summary"
  echo
  echo "  Go ${REQUIRED_GO_VERSION}+     → $(command -v go &>/dev/null && go version | grep -oP '\d+\.\d+(\.\d+)?' | head -1 || echo 'NOT FOUND')"
  echo "  Ollama         → $(command -v ollama &>/dev/null && ollama --version 2>/dev/null | grep -oP '\d+\.\d+\.\d+' | head -1 || echo 'NOT FOUND')"
  if [ "$PLATFORM" = "linux" ]; then
    echo "  libsecret      → $(ldconfig -p 2>/dev/null | grep -q libsecret && echo 'present' || echo 'NOT FOUND')"
    echo "  libX11         → $(ldconfig -p 2>/dev/null | grep -q libX11 && echo 'present' || echo 'NOT FOUND')"
    echo "  libatspi       → $(ldconfig -p 2>/dev/null | grep -q libatspi && echo 'present' || echo 'NOT FOUND')"
  fi
  echo
  if ! $SKIP_MODEL && command -v ollama &>/dev/null && curl -sf "http://127.0.0.1:11434/api/tags" &>/dev/null; then
    local model_name="${DEFAULT_OLLAMA_MODEL%%:*}"
    echo "  ${DEFAULT_OLLAMA_MODEL} → $(ollama list 2>/dev/null | grep -q "^${model_name}" && echo 'pulled' || echo 'NOT PULLED')"
  fi
  echo
  echo "  To build:  cd ${REPO_ROOT} && go build -o bin/digitalghost ./cmd/digitalghost"
  echo "  To run:    ./bin/digitalghost"
  echo "  To wipe:   ./bin/digitalghost --wipe --confirm"
  echo
}

# ── Main ───────────────────────────────────────────────────────────────────────
main() {
  echo
  echo "${BOLD}Digital Ghost — Prerequisites Setup${RESET}"
  echo "────────────────────────────────────────"
  if $CHECK_ONLY; then
    echo "${YELLOW}Running in CHECK ONLY mode — nothing will be installed${RESET}"
  fi
  echo

  detect_platform
  check_root_guard
  check_python3
  check_curl
  check_git
  check_build_tools

  # Platform-specific system libraries (must come before Go, which may need cgo).
  if [ "$PLATFORM" = "linux" ]; then
    pkg_update_index
    check_linux_keychain_deps
    check_linux_x11_deps
    check_linux_wayland_deps
  elif [ "$PLATFORM" = "macos" ]; then
    check_macos_deps
  fi

  check_go
  check_ollama
  check_ollama_running
  check_ollama_model
  check_go_build
  print_summary
}

main "$@"
