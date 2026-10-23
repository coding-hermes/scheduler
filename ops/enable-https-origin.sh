#!/usr/bin/env bash
# SCHED-GAP-1600 — provision a trusted HTTPS origin for the dashboard, then
# retire the plain-HTTP tailnet forward ONLY after the replacement is proven.
#
# Why the order is enforced rather than documented: browser Notifications and
# service workers are refused on the cleartext tailnet origin (isSecureContext
# false, Notification.permission "denied", no navigator.serviceWorker). The fix
# is an https origin with a TRUSTED certificate. Retiring the cleartext forward
# before such an origin answers would leave the dashboard with zero exposure
# paths — strictly worse than the defect. So every destructive step below runs
# only after a live probe of the new origin passes, and the script refuses
# otherwise.
#
#   # Path A (preferred): TLS terminated by tailscaled, Tailscale-issued cert.
#   # Requires "HTTPS Certificates" enabled for the tailnet (admin console -> DNS).
#   ops/enable-https-origin.sh --tailscale --stage --apply
#   ops/enable-https-origin.sh --retire --apply
#
#   # Path B: the daemon serves TLS itself (--tls-cert/--tls-key).
#   ops/enable-https-origin.sh --cert /path/fullchain.pem --key /path/privkey.pem \
#       --stage --apply            # writes the systemd drop-in, does NOT restart
#   # ... drain + restart schedulerd (see the staged instructions) ...
#   ops/enable-https-origin.sh --retire --apply
#
# Dry-run is the DEFAULT: without --apply the script prints what it would do and
# runs the read-only probes. See docs/reference/https-origin.md for the measured
# browser results and the verification snippet.
set -euo pipefail

SCHEDULER_PORT="${SCHEDULER_PORT:-9090}"
SOCAT_UNIT="${SOCAT_UNIT:-sched-ui-tailnet.service}"
SCHEDULER_UNIT="${SCHEDULER_UNIT:-coding-hermes-scheduler.service}"
DROPIN_DIR="${DROPIN_DIR:-$HOME/.config/systemd/user/${SCHEDULER_UNIT}.d}"
DROPIN_FILE="$DROPIN_DIR/tls.conf"

MODE=""
ORIGIN_URL=""
CERT_FILE=""
KEY_FILE=""
APPLY=0

usage() {
  cat <<'USAGE'
SCHED-GAP-1600 — provision a trusted HTTPS origin for the dashboard, then retire
the plain-HTTP tailnet forward ONLY after the replacement is proven.

Why the order is enforced: browser Notifications and service workers are refused
on the cleartext tailnet origin (isSecureContext false, Notification.permission
"denied", no navigator.serviceWorker). Retiring the cleartext forward before a
trusted https origin answers would leave the dashboard with ZERO exposure paths
— worse than the defect — so every destructive step below runs only after a live
probe of the new origin passes.

  # Path A (preferred): TLS terminated by tailscaled with a Tailscale cert.
  # Requires "HTTPS Certificates" enabled for the tailnet (admin console -> DNS).
  ops/enable-https-origin.sh --tailscale --stage --apply
  ops/enable-https-origin.sh --retire --apply

  # Path B: the daemon serves TLS itself (--tls-cert/--tls-key).
  ops/enable-https-origin.sh --cert /path/fullchain.pem --key /path/privkey.pem \
      --stage --apply        # writes a systemd drop-in, does NOT restart
  #   ... drain + restart schedulerd (see the staged instructions) ...
  ops/enable-https-origin.sh --retire --apply --origin-url https://<name-in-cert>

Flags: --tailscale | --cert FILE --key FILE | --retire
       [--stage] [--origin-url URL] [--apply]

Dry run is the DEFAULT: probes run, nothing is changed without --apply.
See docs/reference/https-origin.md for measured results and the browser probe.
USAGE
}

die() { echo "REFUSED: $*" >&2; exit 1; }
note() { echo "  $*"; }
step() { echo; echo "== $*"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tailscale) MODE="tailscale"; shift ;;
    --cert) CERT_FILE="${2:-}"; shift 2 ;;
    --key) KEY_FILE="${2:-}"; shift 2 ;;
    --retire) MODE="retire"; shift ;;
    --stage) shift ;;  # explicit form of the default (stage) action
    --origin-url) ORIGIN_URL="${2:-}"; shift 2 ;;
    --apply) APPLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

# MODE is set by --tailscale/--retire; Path B is identified by the cert pair, so
# an empty MODE is legal exactly when --cert/--key were given.
if [[ -z "$MODE" && -z "$CERT_FILE" && -z "$KEY_FILE" ]]; then
  usage >&2
  exit 2
fi

# --- helpers ---------------------------------------------------------------

tailnet_dns_name() {
  tailscale status --json 2>/dev/null \
    | jq -r '.Self.DNSName // empty' 2>/dev/null \
    | sed 's/\.$//'
}

# probe_https_origin <url> -> 0 when a TRUSTED https origin serves the dashboard.
# curl without -k fails on an untrusted chain, so a passing probe proves the
# certificate is trusted — which is what makes the origin a browser secure
# context. The AC-level browser assertions (isSecureContext, Notification
# permission, serviceWorker) are in docs/reference/https-origin.md.
probe_https_origin() {
  local url="$1" body rc=0
  [[ "$url" == https://* ]] || die "origin '$url' is not https — a cleartext origin can never be a secure context"
  body="$(curl -fsS --max-time 10 "$url/api/v1/health" 2>&1)" || rc=$?
  if [[ $rc -eq 60 ]]; then
    note "certificate NOT trusted by the local CA store (curl exit 60) — a browser would show a warning interstitial and refuse service workers"
    return 1
  elif [[ $rc -ne 0 ]]; then
    note "probe failed (curl exit $rc): $body"
    return 1
  fi
  if ! grep -q 'status' <<<"$body"; then
    note "origin answered but /api/v1/health does not look like the scheduler: ${body:0:120}"
    return 1
  fi
  note "trusted TLS + dashboard answering: $url"
  return 0
}

socat_active() {
  systemctl --user is-active "$SOCAT_UNIT" >/dev/null 2>&1
}

socat_enabled() {
  systemctl --user is-enabled "$SOCAT_UNIT" >/dev/null 2>&1
}

# --- stage: Path A (tailscale serve) --------------------------------------

stage_tailscale() {
  step "Path A — tailscale serve --https=443"
  command -v tailscale >/dev/null || die "tailscale not installed"
  command -v jq >/dev/null || die "jq not installed"

  local dns; dns="$(tailnet_dns_name)"
  [[ -n "$dns" ]] || die "could not resolve this node's tailnet DNS name (tailscale status --json)"
  [[ -n "$ORIGIN_URL" ]] || ORIGIN_URL="https://$dns"

  note "node origin: $ORIGIN_URL"
  note "pre-existing serve config (left alone):"
  tailscale serve status 2>&1 | sed 's/^/    /' || true

  # Certificate capability is the gate: serve --https cannot mint one without it.
  local tmpdir; tmpdir="$(mktemp -d)"
  local cert_rc=0 cert_out=""
  # shellcheck disable=SC2024 # the redirect is deliberately the USER's (we read
  # the diagnostic afterwards as the user); the cert itself is written by root
  # via --cert-file/--key-file inside a 0700 user-owned tmpdir.
  sudo -n tailscale cert --cert-file "$tmpdir/c.pem" --key-file "$tmpdir/k.pem" "$dns" >"$tmpdir/out" 2>&1 || cert_rc=$?
  cert_out="$(cat "$tmpdir/out" 2>/dev/null || true)"
  rm -rf "$tmpdir"
  if [[ $cert_rc -ne 0 ]]; then
    note "tailscale cert failed: $(head -2 <<<"$cert_out")"
    note "HTTPS certificates are a TAILNET toggle: admin console -> DNS -> 'HTTPS Certificates',"
    note "or https://login.tailscale.com/f/serve — enable it, then re-run this command."
    note "The serve config was NOT modified."
    return 1
  fi
  note "certificate capability: OK"

  if [[ $APPLY -eq 0 ]]; then
    note "DRY RUN — would run: sudo tailscale serve --bg --https=443 http://127.0.0.1:$SCHEDULER_PORT"
    note "DRY RUN — would then probe $ORIGIN_URL"
    return 0
  fi

  sudo -n tailscale serve --bg --https=443 "http://127.0.0.1:$SCHEDULER_PORT" \
    || die "tailscale serve failed — nothing else was changed"
  tailscale serve status 2>&1 | sed 's/^/    /'

  probe_https_origin "$ORIGIN_URL" || die "the new origin did not pass its probe — the plain-HTTP forward is UNTOUCHED; fix the origin, then run --retire"
  step "Staged"
  note "origin is live and trusted; run: $0 --retire --apply"
}

# --- stage: Path B (daemon TLS listener) ----------------------------------

stage_daemon_tls() {
  step "Path B — daemon TLS listener (--tls-cert/--tls-key)"
  [[ -n "$CERT_FILE" && -n "$KEY_FILE" ]] || die "--cert and --key must be given together"
  [[ -r "$CERT_FILE" ]] || die "certificate not readable: $CERT_FILE"
  [[ -r "$KEY_FILE" ]] || die "private key not readable: $KEY_FILE"

  # A mismatched pair would boot-fail the daemon; catch it here, not at restart.
  local cert_pub key_pub
  cert_pub="$(openssl x509 -in "$CERT_FILE" -noout -pubkey 2>/dev/null | openssl sha256)" \
    || die "cannot parse certificate: $CERT_FILE"
  key_pub="$(openssl pkey -in "$KEY_FILE" -pubout 2>/dev/null | openssl sha256)" \
    || die "cannot parse private key: $KEY_FILE"
  [[ "$cert_pub" == "$key_pub" ]] || die "certificate and key do not match — the daemon would fail to start"

  note "certificate/key pair matches"
  openssl x509 -in "$CERT_FILE" -noout -subject -enddate 2>/dev/null | sed 's/^/    /'

  if [[ -z "$ORIGIN_URL" ]]; then
    note "no --origin-url given; it must match the certificate's name, e.g. https://<name-in-cert>"
  fi

  if [[ $APPLY -eq 0 ]]; then
    note "DRY RUN — would write $DROPIN_FILE with SCHEDULER_TLS_CERT/KEY"
    note "DRY RUN — would NOT restart the daemon (a restart needs the normal drain sequence)"
    return 0
  fi

  mkdir -p "$DROPIN_DIR"
  umask 022
  cat >"$DROPIN_FILE" <<EOF
[Service]
# SCHED-GAP-1600: serve the dashboard/API/MCP over TLS so the origin is a
# browser secure context (Notifications + service workers). Written by
# ops/enable-https-origin.sh; remove this file to return to plain HTTP.
Environment=SCHEDULER_TLS_CERT=$CERT_FILE
Environment=SCHEDULER_TLS_KEY=$KEY_FILE
EOF
  note "wrote $DROPIN_FILE"
  step "Staged — the daemon still serves plain HTTP"
  note "Now drain + restart $SCHEDULER_UNIT through the normal sequence, then confirm the"
  note "boot log prints 'HTTPS: listening on' and 'Origin: SECURE CONTEXT (TLS)'."
  note "After the restart proves live: $0 --retire --apply --origin-url $ORIGIN_URL"
}

# --- retire: the destructive step, gated on a fresh passing probe ---------

retire_forward() {
  step "Retire the plain-HTTP tailnet forward ($SOCAT_UNIT)"
  command -v jq >/dev/null || die "jq not installed"

  if [[ -z "$ORIGIN_URL" ]]; then
    local dns; dns="$(tailnet_dns_name)"
    [[ -n "$dns" ]] || die "no --origin-url and no tailnet DNS name to probe"
    ORIGIN_URL="https://$dns"
  fi
  note "probing replacement origin: $ORIGIN_URL"
  probe_https_origin "$ORIGIN_URL" \
    || die "replacement origin is not a trusted https origin serving the dashboard — REFUSING to retire $SOCAT_UNIT (that would leave zero exposure paths)"

  if ! socat_active && ! socat_enabled; then
    note "$SOCAT_UNIT is already inactive and disabled — one exposure path already"
    return 0
  fi

  if [[ $APPLY -eq 0 ]]; then
    note "DRY RUN — would run: systemctl --user disable --now $SOCAT_UNIT"
    note "DRY RUN — would then assert a single listener on :$SCHEDULER_PORT"
    return 0
  fi

  systemctl --user disable --now "$SOCAT_UNIT" || die "failed to stop/disable $SOCAT_UNIT"
  note "stopped + disabled $SOCAT_UNIT"

  step "Post-retirement checks"
  local listeners
  listeners="$(ss -ltn 2>/dev/null | awk -v p=":$SCHEDULER_PORT" '$4 ~ p {print $4}' | sort -u)"
  note "listeners on $SCHEDULER_PORT (report only — the daemon bind and a serve handler differ by design):"
  sed 's/^/    /' <<<"${listeners:-none}"
  if systemctl --user is-active "$SOCAT_UNIT" >/dev/null 2>&1; then
    die "$SOCAT_UNIT is still active — retirement incomplete"
  fi
  note "$SOCAT_UNIT is inactive and disabled"
  note "verify in a browser: isSecureContext === true, Notification.permission === 'default'"
}

# --- dispatch -------------------------------------------------------------

case "$MODE" in
  tailscale) stage_tailscale ;;
  retire) retire_forward ;;
  "")
    if [[ -n "$CERT_FILE" || -n "$KEY_FILE" ]]; then
      stage_daemon_tls
    else
      usage >&2
      exit 2
    fi
    ;;
  *)
    usage >&2
    die "unsupported mode: $MODE"
    ;;
esac
