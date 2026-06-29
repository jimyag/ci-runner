#!/usr/bin/env bash
# Start the smee client based on .smee-url.
#
# .smee-url is per-developer local state. When it is missing, this script exits
# successfully so webhook forwarding stays opt-in for local development.
set -euo pipefail

URL_FILE=".smee-url"
TARGET="${SMEE_TARGET:-http://127.0.0.1:25500/webhooks/github}"

CHECK_ONLY=0
if [ "${1:-}" = "--check" ]; then
  CHECK_ONLY=1
fi

if [ ! -f "$URL_FILE" ]; then
  if [ "$CHECK_ONLY" = "1" ]; then
    exit 0
  fi

  cat <<'EOF'
[smee] .smee-url not found; skipping webhook forwarder.

To enable GitHub webhook forwarding:

  1. Get a channel URL: open https://smee.io/new
  2. Save it to .smee-url: echo 'https://smee.io/<your-channel>' > .smee-url
  3. Configure the same URL as the GitHub webhook URL.
  4. Run task smee.

Set SMEE_TARGET if runnerd is not listening on http://127.0.0.1:25500.
EOF
  exit 0
fi

URL="$(head -n1 "$URL_FILE" | tr -d '[:space:]')"
if [ -z "$URL" ]; then
  echo "[smee] .smee-url is empty; refusing to start." >&2
  exit 1
fi

if ! command -v npx >/dev/null 2>&1; then
  echo "[smee] npx is required to run smee-client. Install Node.js/npm first." >&2
  exit 1
fi

if [ "$CHECK_ONLY" = "1" ]; then
  npx --yes smee-client --help >/dev/null
  exit 0
fi

echo "[smee] forwarding ${URL} -> ${TARGET}"
exec npx --yes smee-client --url "$URL" --target "$TARGET"
