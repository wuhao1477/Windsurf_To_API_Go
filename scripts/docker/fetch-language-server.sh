#!/bin/sh

set -eu

DEST_PATH="${1:-/app/bin/language_server_linux_x64}"
DOWNLOAD_URL="${LS_DOWNLOAD_URL:?LS_DOWNLOAD_URL is required}"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_DIR"
}

trap cleanup EXIT INT TERM

mkdir -p "$(dirname "$DEST_PATH")"

curl -fsSL "$DOWNLOAD_URL" -o "$TMP_DIR/windsurf.tar.gz"
tar -xzf "$TMP_DIR/windsurf.tar.gz" -C "$TMP_DIR"

cp \
  "$TMP_DIR/Windsurf/resources/app/extensions/windsurf/bin/language_server_linux_x64" \
  "$DEST_PATH"

chmod +x "$DEST_PATH"
