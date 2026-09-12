#!/usr/bin/env bash
# One-time root bootstrap; application permissions and service unit stay intact.
set -Eeuo pipefail
[[ $(id -u) == 0 && $# == 0 ]]
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]]
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
getent passwd sub2api >/dev/null
for cmd in python3 openssl systemctl age jq; do command -v "$cmd" >/dev/null; done
openssl pkey -pubin -in "$source_dir/bud-release-public.pem" -noout
if [[ -e /etc/sub2api/bud-release-public.pem ]]; then
    # Key changes require an explicit, separate rotation; never silently trust a new publisher.
    cmp "$source_dir/bud-release-public.pem" /etc/sub2api/bud-release-public.pem
fi
install -d -o root -g sub2api -m 0750 /var/lib/sub2api-updater
install -o root -g root -m 0644 "$source_dir/bud-release-public.pem" /etc/sub2api/bud-release-public.pem
install -o root -g root -m 0755 "$source_dir/bud-updater.py" /usr/local/libexec/sub2api-bud-update
install -o root -g root -m 0644 "$source_dir/bud-update.socket" /etc/systemd/system/bud-update.socket
install -o root -g root -m 0644 "$source_dir/bud-update@.service" /etc/systemd/system/bud-update@.service
systemctl daemon-reload
systemctl enable --now bud-update.socket
systemctl is-active --quiet bud-update.socket
[[ $(stat -c '%U:%G:%a' /run/sub2api-update.sock) == root:sub2api:660 ]]
echo 'Signed Bud installer ready. Main application was not restarted.'
