#!/usr/bin/env bash
# Builds local/wame and installs/recreates the Unraid container on tardis.
# Run from the checkout on tardis: /mnt/nvme_cache/appdata/wame-src/unraid/install.sh
# The owner secret is kept from the installed template; on first install it comes from
# $WAME_TOKEN or is generated (copy it into Infisical project "wame" afterwards).
set -euo pipefail
cd "$(dirname "$0")/.."

name=wame
data=/mnt/nvme_cache/appdata/wame
templates=/boot/config/plugins/dockerMan/templates-user
template=$templates/my-$name.xml
autostart=/var/lib/docker/unraid-autostart

version=$(git describe --tags --always --dirty 2>/dev/null || date +%Y%m%d)
docker build -t "local/wame:$version" -t local/wame:latest --build-arg VERSION="$version" .

mkdir -p "$data"
chown 99:100 "$data"
chmod 700 "$data"

token=$(sed -n 's:.*Target="WAME_TOKEN".*>\(.*\)</Config>.*:\1:p' "$template" 2>/dev/null || true)
if [ -z "$token" ]; then
  token=${WAME_TOKEN:-$(openssl rand -hex 24)}
  echo "New owner secret written to $template (WAME_TOKEN). Store it in Infisical project \"wame\"."
fi
[ -f "$template" ] && cp "$template" "$template.bak-$(date +%Y%m%d-%H%M%S)"
tmp=$(mktemp)
sed "s:\(Target=\"WAME_TOKEN\".*>\)</Config>:\1$token</Config>:" unraid/my-$name.xml > "$tmp"
install -m 600 "$tmp" "$template"
rm -f "$tmp"

/usr/local/emhttp/plugins/dynamix.docker.manager/scripts/rebuild_container "$name"
grep -q "^$name\( \|$\)" "$autostart" 2>/dev/null || echo "$name 0" >> "$autostart"
docker start "$name" >/dev/null
docker ps --filter "name=^$name$" --format '{{.Names}}: {{.Status}} ({{.Image}})'
