#!/bin/sh
#
# Installs a FitHalo Scoreboard as a systemd service on Linux, a Raspberry Pi included.
#
#   sudo ./install.sh https://board.example.com proxy
#   sudo ./install.sh https://board.example.com files
#
# proxy: the board listens on 127.0.0.1:8080 for a tunnel or proxy on this machine (Tailscale Funnel,
#        Cloudflare Tunnel, Caddy) that terminates TLS.
# files: the board serves TLS itself on port 443 from /etc/fithalo-scoreboard/tls, which certbot fills.
#
# Running it again updates the binary and the service and keeps the existing configuration and pepper.
set -eu

readonly SERVICE_USER='fithalo-scoreboard'
readonly CONFIGURATION_DIR='/etc/fithalo-scoreboard'
readonly DROP_IN_DIR='/etc/systemd/system/fithalo-scoreboard.service.d'

public_url="${1:-}"
mode="${2:-proxy}"
if [ -z "${public_url}" ] || { [ "${mode}" != proxy ] && [ "${mode}" != files ]; }; then
  echo "Usage: sudo $0 https://board.example.com [proxy|files]" >&2
  exit 2
fi
case "${public_url}" in
  https://*) ;;
  *)
    echo "The board's address must start with https://" >&2
    exit 2
    ;;
esac
if [ "$(id -u)" -ne 0 ]; then
  echo "Run this with sudo." >&2
  exit 1
fi

here="$(cd "$(dirname "$0")" && pwd)"

install -m 0755 "${here}/fithalo-scoreboard" /usr/local/bin/fithalo-scoreboard
if ! id "${SERVICE_USER}" >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/fithalo-scoreboard --no-create-home \
    --shell /usr/sbin/nologin "${SERVICE_USER}"
fi
install -d -m 0750 -o root -g "${SERVICE_USER}" "${CONFIGURATION_DIR}"
install -d -m 0750 -o root -g "${SERVICE_USER}" "${CONFIGURATION_DIR}/tls"

# The pepper lives apart from the data, so a copy of the data alone cannot be used to guess the
# admin's password.
if [ ! -f "${CONFIGURATION_DIR}/pepper" ]; then
  (umask 027 && head -c 32 /dev/urandom | base64 > "${CONFIGURATION_DIR}/pepper")
  chgrp "${SERVICE_USER}" "${CONFIGURATION_DIR}/pepper"
fi

if [ ! -f "${CONFIGURATION_DIR}/environment" ]; then
  setup_code="$(head -c 18 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  if [ "${mode}" = files ]; then
    listen=0.0.0.0:443
    certificates="SCOREBOARD_CERT=${CONFIGURATION_DIR}/tls/fullchain.pem
SCOREBOARD_KEY=${CONFIGURATION_DIR}/tls/privkey.pem"
  else
    listen=127.0.0.1:8080
    certificates=""
  fi
  (umask 027 && cat > "${CONFIGURATION_DIR}/environment" <<CONFIGURATION
SCOREBOARD_PUBLIC_URL=${public_url}
SCOREBOARD_TLS=${mode}
SCOREBOARD_LISTEN=${listen}
SCOREBOARD_DATA=/var/lib/fithalo-scoreboard
SCOREBOARD_PEPPER_FILE=${CONFIGURATION_DIR}/pepper
SCOREBOARD_SETUP_CODE=${setup_code}
${certificates}
CONFIGURATION
  )
  chgrp "${SERVICE_USER}" "${CONFIGURATION_DIR}/environment"
  echo "Set up the board's admin at ${public_url}/admin/setup with the setup code ${setup_code}"
fi

install -m 0644 "${here}/fithalo-scoreboard.service" /etc/systemd/system/fithalo-scoreboard.service

# Only files mode binds port 443; the kept configuration decides, not the argument.
serving_mode="$(sed -n 's/^SCOREBOARD_TLS=//p' "${CONFIGURATION_DIR}/environment" | tail -n 1)"
if [ "${serving_mode}" = files ]; then
  install -d -m 0755 "${DROP_IN_DIR}"
  cat > "${DROP_IN_DIR}/bind-port-443.conf" <<DROPIN
# Written by install.sh: the board serves TLS itself on port 443.
[Service]
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
DROPIN
  chmod 0644 "${DROP_IN_DIR}/bind-port-443.conf"
else
  rm -f "${DROP_IN_DIR}/bind-port-443.conf"
  rmdir "${DROP_IN_DIR}" 2>/dev/null || true
fi
systemctl daemon-reload
if [ "${serving_mode}" = files ] && [ ! -f "${CONFIGURATION_DIR}/tls/fullchain.pem" ]; then
  systemctl enable fithalo-scoreboard
  echo "Installed. Get a certificate into ${CONFIGURATION_DIR}/tls (see the guide), then: sudo systemctl start fithalo-scoreboard"
else
  systemctl enable --now fithalo-scoreboard
  systemctl restart fithalo-scoreboard
  echo "Running. Logs: journalctl -u fithalo-scoreboard"
fi
