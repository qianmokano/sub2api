#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != 0 ]]; then
  echo 'Run this installer as root.' >&2
  exit 1
fi
if systemctl is-active --quiet kano-updater.service; then
  echo 'Updater is already running. Follow the updater replacement procedure in KANO-UPDATER.md.' >&2
  exit 1
fi

package_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
config_path=/etc/kano-updater/config.json
install -d -m 700 /etc/kano-updater /var/lib/kano-updater
if [[ ! -e "$config_path" ]]; then
  install -m 600 "$package_dir/config.example.json" "$config_path"
fi

# The supplied profile is the existing Kano gateway deployment. Review a custom
# profile before invoking this installer. The Go command validates fixed paths,
# adds only its Compose overlay and checks incremental Nginx edits before reload.
"$package_dir/kano-updater" install --config "$config_path" --nginx-site "${1:-/etc/nginx/sites-enabled/sub2api}"
install -m 755 "$package_dir/kano-updater" /usr/local/bin/kano-updater
install -m 644 "$package_dir/kano-updater.service" /etc/systemd/system/kano-updater.service
systemctl daemon-reload
systemctl enable --now kano-updater.service
nginx -t
systemctl reload nginx

# First-time wiring recreates the same app image to mount the control directory
# and enable the client. Database/Redis services and credentials stay in place.
docker compose --project-directory /opt/sub2api-deploy \
  -f /opt/sub2api-deploy/docker-compose.yml \
  -f /opt/sub2api-deploy/kano-updater.compose.json \
  -f /opt/sub2api-deploy/kano-image.override.json up -d --no-deps sub2api

for attempt in {1..90}; do
  if curl --silent --fail --max-time 5 http://127.0.0.1:8081/health; then
    echo
    echo 'Updater installed; original application is healthy.'
    exit 0
  fi
  sleep 1
done
echo 'Application health needs inspection; updater and installation backups are retained.' >&2
exit 1
