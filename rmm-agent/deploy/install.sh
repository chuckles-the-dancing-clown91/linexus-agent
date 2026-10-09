#!/bin/sh
# Reference installer for the Linexus agent.
#
# Nexus serves the canonical copy at $NEXUS_PUBLIC_URL/install/agent.sh; this
# file documents what that script must do and works standalone:
#
#   curl -fsSL $NEXUS/install/agent.sh | NEXUS_URL=$NEXUS ENROLLMENT_TOKEN=nxe_… sh
#
# Optional, written into the env file when given (Nexus's rendered script
# fills the first two in):
#   LINEXUS_SIGNING_PUBKEY  Nexus's plan-signing Ed25519 public key (base64);
#                           the agent refuses plans not signed with it
#   LINEXUS_CA_FILE         PEM bundle that signed Nexus's certificate (a
#                           private CA); also used for the download below
#   LINEXUS_CA_PEM          the bundle's content instead: written to
#                           /etc/linexus/nexus-ca.pem, which becomes
#                           LINEXUS_CA_FILE
#   LINEXUS_CA_ONLY, LINEXUS_CLIENT_CERT, LINEXUS_CLIENT_KEY,
#   LINEXUS_ALLOW_INSECURE, LINEXUS_ALLOW_UNSIGNED — see the README
#
# Steps, all idempotent (re-running upgrades the binary and keeps the
# agent's identity):
#   1. download rmm-agent-linux-<amd64|arm64> from $AGENT_BINARY_URL
#      (default $NEXUS_URL/install) to /usr/local/bin/linexus-agent;
#   2. write /etc/linexus/agent.env (0600) with NEXUS_URL, ENROLLMENT_TOKEN,
#      AGENT_STATE_FILE=/var/lib/linexus/agent-state.json and the optional
#      LINEXUS_* settings above, keeping any other lines an operator added;
#   3. install /etc/systemd/system/linexus-agent.service, enable and
#      (re)start it.
# Once enrolled, the agent ignores ENROLLMENT_TOKEN: its identity and
# per-agent credential live in the state file.
set -eu

die() { echo "linexus-agent install: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (pipe to 'sudo sh')"
[ -n "${NEXUS_URL:-}" ] || die "NEXUS_URL is required"
NEXUS_URL=${NEXUS_URL%/}
STATE_FILE=/var/lib/linexus/agent-state.json
if [ -z "${ENROLLMENT_TOKEN:-}" ] && [ ! -s "$STATE_FILE" ] && [ -z "${AGENT_TOKEN:-}" ]; then
  die "ENROLLMENT_TOKEN is required for a first install"
fi
command -v systemctl >/dev/null 2>&1 || die "systemd is required"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

install -d -m 0755 /etc/linexus
if [ -n "${LINEXUS_CA_PEM:-}" ]; then
  printf '%s\n' "$LINEXUS_CA_PEM" > /etc/linexus/nexus-ca.pem.new
  chmod 0644 /etc/linexus/nexus-ca.pem.new
  mv -f /etc/linexus/nexus-ca.pem.new /etc/linexus/nexus-ca.pem
  LINEXUS_CA_FILE=/etc/linexus/nexus-ca.pem
fi
if [ -n "${LINEXUS_CA_FILE:-}" ] && [ ! -r "$LINEXUS_CA_FILE" ]; then
  die "LINEXUS_CA_FILE $LINEXUS_CA_FILE is not readable"
fi

BASE=${AGENT_BINARY_URL:-$NEXUS_URL/install}
BIN=/usr/local/bin/linexus-agent
TMP=$(mktemp)
trap 'rm -f "$TMP" "$TMP.env"' EXIT

echo "downloading $BASE/rmm-agent-linux-$ARCH"
if command -v curl >/dev/null 2>&1; then
  set -- -fsSL --retry 5 --retry-delay 3
  if [ -n "${LINEXUS_CA_FILE:-}" ]; then set -- "$@" --cacert "$LINEXUS_CA_FILE"; fi
  curl "$@" -o "$TMP" "$BASE/rmm-agent-linux-$ARCH"
elif command -v wget >/dev/null 2>&1; then
  set -- -q
  if [ -n "${LINEXUS_CA_FILE:-}" ]; then set -- "$@" --ca-certificate="$LINEXUS_CA_FILE"; fi
  wget "$@" -O "$TMP" "$BASE/rmm-agent-linux-$ARCH"
else
  die "curl or wget is required"
fi
# An ELF binary, not an error page.
[ "$(head -c 4 "$TMP" | od -An -c | tr -d ' ')" = "177ELF" ] || die "download is not a Linux binary"

changed=0
if ! cmp -s "$TMP" "$BIN" 2>/dev/null; then
  install -m 0755 "$TMP" "$BIN.new" && mv -f "$BIN.new" "$BIN"
  changed=1
fi

install -d -m 0700 /var/lib/linexus
ENV=/etc/linexus/agent.env
managed='NEXUS_URL|ENROLLMENT_TOKEN|AGENT_STATE_FILE'
if [ -n "${AGENT_TOKEN:-}" ]; then managed="$managed|AGENT_TOKEN"; fi
if [ -n "${AGENT_HOSTGROUP:-}" ]; then managed="$managed|AGENT_HOSTGROUP"; fi
# Optional LINEXUS_* settings: each one given replaces its line in the file.
OPTIONAL_VARS="LINEXUS_SIGNING_PUBKEY LINEXUS_CA_FILE LINEXUS_CA_ONLY LINEXUS_CLIENT_CERT LINEXUS_CLIENT_KEY LINEXUS_ALLOW_INSECURE LINEXUS_ALLOW_UNSIGNED"
for v in $OPTIONAL_VARS; do
  eval "val=\${$v:-}"
  if [ -n "$val" ]; then managed="$managed|$v"; fi
done
{
  echo "# Written by the Linexus agent installer. Other lines are kept on re-install."
  echo "NEXUS_URL=$NEXUS_URL"
  if [ -n "${ENROLLMENT_TOKEN:-}" ]; then echo "ENROLLMENT_TOKEN=$ENROLLMENT_TOKEN"; fi
  if [ -n "${AGENT_TOKEN:-}" ]; then echo "AGENT_TOKEN=$AGENT_TOKEN"; fi
  if [ -n "${AGENT_HOSTGROUP:-}" ]; then echo "AGENT_HOSTGROUP=$AGENT_HOSTGROUP"; fi
  echo "AGENT_STATE_FILE=$STATE_FILE"
  for v in $OPTIONAL_VARS; do
    eval "val=\${$v:-}"
    if [ -n "$val" ]; then echo "$v=$val"; fi
  done
  if [ -f "$ENV" ]; then
    grep -v -E "^(# Written by the Linexus agent installer|($managed)=)" "$ENV" || true
  fi
} > "$TMP.env"
if ! cmp -s "$TMP.env" "$ENV" 2>/dev/null; then
  install -m 0600 "$TMP.env" "$ENV"
  changed=1
fi

UNIT=/etc/systemd/system/linexus-agent.service
cat > "$TMP" <<'UNIT'
[Unit]
Description=Linexus infrastructure agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/linexus/agent.env
Environment=AGENT_STATE_FILE=/var/lib/linexus/agent-state.json
ExecStart=/usr/local/bin/linexus-agent
StateDirectory=linexus
StateDirectoryMode=0700
WorkingDirectory=/var/lib/linexus
UMask=0022
Restart=always
RestartSec=10
TimeoutStopSec=30
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
if ! cmp -s "$TMP" "$UNIT" 2>/dev/null; then
  install -m 0644 "$TMP" "$UNIT"
  changed=1
fi

systemctl daemon-reload
systemctl enable linexus-agent.service >/dev/null 2>&1
if [ "$changed" -eq 1 ] || ! systemctl is-active --quiet linexus-agent.service; then
  systemctl restart linexus-agent.service
fi
echo "linexus-agent installed and running — journalctl -u linexus-agent -f"
