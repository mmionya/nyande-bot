#!/usr/bin/env bash
# Install a published Linux x86-64 release into the existing native service.
set -euo pipefail

if [[ $# != 2 || ! $1 =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ || ! $2 =~ ^[a-fA-F0-9]{64}$ ]]; then
    echo "Usage: bash install-release.sh RELEASE_TAG SHA256" >&2
    exit 2
fi
if [[ $EUID != 0 || $(uname -s) != Linux || $(uname -m) != x86_64 ]]; then
    echo "Run this installer as root on Linux x86_64." >&2
    exit 1
fi

unit=nyande-bot.service
binary=/usr/local/bin/nyande-bot
asset_url="https://github.com/mmionya/nyande-bot/releases/download/$1/nyande-bot-linux-amd64"
backup_root=/var/backups/nyande-bot
umask 077
exec 9>/run/lock/nyande-bot-update.lock
flock -n 9 || { echo "Another bot update is running." >&2; exit 1; }

main_pid() { systemctl show "$unit" -p MainPID --value; }
running_binary() {
    local pid=$1
    [[ $pid =~ ^[1-9][0-9]*$ && /proc/$pid/exe -ef $binary ]] &&
        systemctl is-active --quiet "$unit"
}

if [[ ! -f $binary || -L $binary ]] || ! running_binary "$(main_pid)"; then
    echo "Expected $unit to be running $binary." >&2
    exit 1
fi

staged=$(mktemp "$binary.new.XXXXXXXX")
restore=false
finish() {
    local status=$?
    if [[ $restore == true ]]; then
        echo "Installation failed. Restoring $backup." >&2
        if ! { cp -p -- "$backup" "$staged" && mv -fT -- "$staged" "$binary" &&
               systemctl restart "$unit" && running_binary "$(main_pid)"; }; then
            echo "Rollback failed. The previous binary is at $backup." >&2
            status=1
        fi
    fi
    rm -f -- "$staged"
    exit "$status"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --retry 3 --output "$staged" "$asset_url"
printf '%s  %s\n' "$2" "$staged" | sha256sum --check --status || {
    echo "Release binary SHA256 mismatch." >&2; exit 1;
}
# Check ELF signature, 64-bit little-endian encoding and amd64 machine type.
if [[ $(od -An -tx1 -N6 "$staged" | tr -d ' \n') != 7f454c460201 ||
      $(od -An -tx1 -j18 -N2 "$staged" | tr -d ' \n') != 3e00 ]]; then
    echo "Expected an amd64 ELF executable." >&2
    exit 1
fi
chmod 0755 "$staged"
mkdir -p "$backup_root"
backup=$(mktemp -d "$backup_root/release.XXXXXXXX")/nyande-bot
cp -p -- "$binary" "$backup"
restore=true
mv -fT -- "$staged" "$binary"
systemctl restart "$unit"
pid=$(main_pid)
for ((i=0; i<15; i++)); do
    sleep 2
    if [[ $(main_pid) != "$pid" ]] || ! running_binary "$pid"; then
        echo "$unit failed its startup check." >&2
        exit 1
    fi
done
restore=false
echo "Installed $1. Previous binary: $backup"
