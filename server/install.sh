#!/bin/sh
# Install a Gliph server on Linux and start it.
#
#   curl -fsSL https://gliph.sh/install.sh | sudo sh -s chat.example.com
#
# Point the domain's DNS at this machine first, and leave ports 80 and 443
# open: Gliph gets its own certificate from Let's Encrypt.
#
# What it does: downloads gliph and its web client into /opt/gliph, makes a
# `gliph` system user, keeps all data in /var/lib/gliph and runs it as the
# systemd service `gliph`. Run it again to upgrade; the data stays.
set -eu

domain=${1:-${GLIPH_DOMAIN:-}}
base=${GLIPH_FROM:-https://gliph.sh}
prefix=/opt/gliph
data=/var/lib/gliph

die() { echo "gliph: $*" >&2; exit 1; }

[ -n "$domain" ] || die "name your domain: curl -fsSL $base/install.sh | sudo sh -s chat.example.com"
[ "$(uname -s)" = Linux ] || die "the server runs on Linux"
[ "$(id -u)" = 0 ] || die "run it as root (pipe it to sudo sh)"
command -v systemctl >/dev/null || die "this script needs systemd"
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "no build for $(uname -m)" ;;
esac

echo "gliph: downloading the server for linux/$arch"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/install/gliph-linux-$arch.tar.gz" -o "$tmp/gliph.tar.gz" ||
  die "no build for linux/$arch at $base yet"
mkdir -p "$tmp/x"
tar -xzf "$tmp/gliph.tar.gz" -C "$tmp/x"
rm -rf "$prefix.new"
mv "$tmp/x" "$prefix.new"
rm -rf "$prefix.old"
[ -d "$prefix" ] && mv "$prefix" "$prefix.old"
mv "$prefix.new" "$prefix"

id gliph >/dev/null 2>&1 || useradd --system --home-dir "$data" --shell /usr/sbin/nologin gliph
mkdir -p "$data"
chown gliph:gliph "$data"

cat >/etc/systemd/system/gliph.service <<EOF
[Unit]
Description=Gliph chat server
After=network-online.target
Wants=network-online.target

[Service]
User=gliph
WorkingDirectory=$data
ExecStart=$prefix/gliph -data $data -web $prefix/web -name $domain -tls-domain $domain
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$data

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable gliph >/dev/null 2>&1
systemctl restart gliph

echo
echo "gliph is running. Open https://$domain and create your account."
echo "The first account is the admin. Logs: journalctl -u gliph -f"
