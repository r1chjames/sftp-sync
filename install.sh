#!/bin/bash
set -euo pipefail

REPO="r1chjames/sftp-sync"
LABEL="com.r1chjames.sftpsyncd"
INSTALL_DIR="/usr/local/bin"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST_FILE="$PLIST_DIR/$LABEL.plist"

# Detect OS and architecture
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

if [ "$OS" != "darwin" ] && [ "$OS" != "linux" ]; then
  echo "Unsupported OS: $OS"
  exit 1
fi

# Resolve version
VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  echo "Fetching latest release..."
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | grep '"tag_name"' | sed 's/.*"tag_name": *"\(.*\)".*/\1/')"
fi
echo "Installing $VERSION ($OS/$ARCH)..."

# Download binaries
BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

for BIN in sftpsyncd sftpsync; do
  echo "  Downloading $BIN..."
  curl -fsSL "$BASE_URL/${BIN}-${OS}-${ARCH}" -o "$TMP/$BIN"
  chmod +x "$TMP/$BIN"
done

# Install binaries
echo "Installing binaries to $INSTALL_DIR (may prompt for sudo)..."
sudo install -m 755 "$TMP/sftpsyncd" "$INSTALL_DIR/sftpsyncd"
sudo install -m 755 "$TMP/sftpsync"  "$INSTALL_DIR/sftpsync"

# macOS-only: install sftpsyncbar.app and LaunchAgent
if [ "$OS" = "darwin" ]; then
  echo "  Downloading sftpsyncbar..."
  curl -fsSL "$BASE_URL/sftpsyncbar-darwin-arm64.zip" -o "$TMP/sftpsyncbar.zip"
  unzip -q "$TMP/sftpsyncbar.zip" -d "$TMP"
  echo "Installing sftpsyncbar.app to /Applications (may prompt for sudo)..."
  sudo rm -rf /Applications/sftpsyncbar.app
  sudo cp -r "$TMP/sftpsyncbar.app" /Applications/sftpsyncbar.app
  xattr -dr com.apple.quarantine /Applications/sftpsyncbar.app
  echo "Installing LaunchAgent..."
  mkdir -p "$PLIST_DIR"
  cat > "$PLIST_FILE" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$INSTALL_DIR/sftpsyncd</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/sftpsyncd.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/sftpsyncd.err</string>
</dict>
</plist>
EOF
  launchctl bootstrap "gui/$(id -u)" "$PLIST_FILE"
  echo ""
  echo "Done. sftpsyncd $VERSION is running and will start at login."
else
  echo ""
  echo "Done. Run sftpsyncd to start the daemon."
fi

echo "  Add a job:  sftpsync add /path/to/config.yaml"
echo "  List jobs:  sftpsync list"
