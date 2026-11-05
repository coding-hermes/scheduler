#!/bin/sh
# install-hooks.sh — copy the repo's hook scripts into ~/.hermes/hooks.
# Idempotent: re-running refreshes the scripts, never touches hooks.toml.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
DEST="${HOOKS_DIR:-$HOME/.hermes/hooks}"
mkdir -p "$DEST"
for f in "$HERE"/*.sh; do
  case "$(basename "$f")" in install-hooks.sh|test-hooks.sh) continue ;; esac
  install -m 0755 "$f" "$DEST/$(basename "$f")"
  echo "installed $DEST/$(basename "$f")"
done
if [ ! -f "$HOME/.hermes/hooks.toml" ]; then
  cp "$HERE/hooks.example.toml" "$HOME/.hermes/hooks.toml"
  echo "created $HOME/.hermes/hooks.toml from the example (edit before arming)"
else
  echo "kept existing $HOME/.hermes/hooks.toml"
fi
