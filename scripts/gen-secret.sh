#!/usr/bin/env bash
#
# gen-secret.sh — Generate cryptographically secure random tokens
# for WEBHOOK_SECRET and JWT_SECRET.
#
# Usage:
#   ./scripts/gen-secret.sh              # generate both WEBHOOK_SECRET and JWT_SECRET
#   ./scripts/gen-secret.sh webhook      # generate WEBHOOK_SECRET only
#   ./scripts/gen-secret.sh jwt          # generate JWT_SECRET only
#   ./scripts/gen-secret.sh --length 48  # custom byte length (default: 32)
#
set -euo pipefail

BYTES=32
TARGET="${1:-all}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --length|-l)
      BYTES="$2"
      shift 2
      ;;
    webhook|jwt|all)
      TARGET="$1"
      shift
      ;;
    *)
      shift
      ;;
  esac
done

generate_token() {
  local bytes="$1"
  # Prefer openssl, fall back to /dev/urandom
  if command -v openssl &>/dev/null; then
    openssl rand -base64 "$bytes" | tr -d '\n'
  else
    head -c "$bytes" /dev/urandom | base64 | tr -d '\n'
  fi
}

generate_webhook_token() {
  local bytes="$1"
  # Telegram secret_token allows only [A-Za-z0-9_-]
  local raw
  if command -v openssl &>/dev/null; then
    raw="$(openssl rand -base64 "$((bytes * 2))" | tr -d '\n')"
  else
    raw="$(head -c "$((bytes * 2))" /dev/urandom | base64 | tr -d '\n')"
  fi
  echo "$raw" | tr -dc 'A-Za-z0-9_-' | head -c "$bytes"
}

print_token() {
  local label="$1"
  local value="$2"
  printf "%-20s %s\n" "$label:" "$value"
}

echo "-------------------------------------------"
echo " SpamObserver — Generated Secrets"
echo "-------------------------------------------"

case "$TARGET" in
  webhook)
    print_token "WEBHOOK_SECRET" "$(generate_webhook_token "$BYTES")"
    ;;
  jwt)
    print_token "JWT_SECRET" "$(generate_token "$BYTES")"
    ;;
  all)
    print_token "WEBHOOK_SECRET" "$(generate_webhook_token "$BYTES")"
    print_token "JWT_SECRET" "$(generate_token "$BYTES")"
    ;;
esac

echo "-------------------------------------------"
echo ""
echo "Paste these into your .env file."
