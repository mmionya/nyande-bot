#!/usr/bin/env bash
# Exercise rollback and runtime settings without root or a running systemd.
set -euo pipefail
# shellcheck source=scripts/update-systemd.sh
source "$(dirname "${BASH_SOURCE[0]}")/update-systemd.sh"
updater=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/update-systemd.sh
test_dir=$(mktemp -d)
trap 'rm -r -- "$test_dir"' EXIT
mkdir "$test_dir/backup" "$test_dir/live"
backup=$test_dir/backup
binary=$test_dir/live/binary
drop_in=$test_dir/live/drop-in
runtime_env=$test_dir/live/runtime.env
unit=nyande-bot.service
systemctl() { printf '%s\n' "$*" >> "$test_dir/systemctl-calls"; }

printf 'old binary\n' > "$backup/binary"
chmod 0755 "$backup/binary"
printf 'original drop-in\n' > "$backup/drop-in"
printf 'new binary\n' > "$binary"
printf 'new drop-in\n' > "$drop_in"
printf 'new runtime env\n' > "$runtime_env"
printf 'BOT_TOKEN=must-stay-unchanged\nWHISPER_ENABLED=true\n' > "$test_dir/live/.env"
before=$(sha256sum "$test_dir/live/.env")
restore_release
cmp "$backup/binary" "$binary"
[[ -x "$binary" ]]
cmp "$backup/drop-in" "$drop_in"
[[ ! -e "$runtime_env" ]]
[[ $(sha256sum "$test_dir/live/.env") == "$before" ]]
[[ $(cat "$test_dir/systemctl-calls") == $'daemon-reload\nrestart nyande-bot.service' ]]

# A second update must restore both of its previous managed files.
printf 'previous runtime env\n' > "$backup/runtime.env"
printf 'new runtime env\n' > "$runtime_env"
restore_release
cmp "$backup/runtime.env" "$runtime_env"

write_runtime_env /opt/nyande-media/runtime.test > "$test_dir/settings"
[[ $(cat "$test_dir/settings") == $'WHISPER_ENABLED=false\nSTICKER_PYTHON_PATH="/opt/nyande-media/runtime.test/bin/python3"' ]]
for bad_path in $'/usr/bin\nINJECTED=value' $'/usr/bin\r' '/usr/bin"' /usr/bin\\; do
    if write_runtime_env "$bad_path" > "$test_dir/rejected"; then
        echo "Accepted an unsafe interpreter path" >&2
        exit 1
    fi
    [[ ! -s "$test_dir/rejected" ]]
done

# The EXIT handler must automatically restore a failed activation, while keeping
# its failure exit status. The production rollback script runs in a fresh shell.
write_rollback_script > "$backup/rollback.sh"
export test_dir
export -f systemctl
printf 'failed new binary\n' > "$binary"
printf 'failed new settings\n' > "$runtime_env"
{
    printf 'source %q\n' "$updater"
    printf 'backup=%q\nactivating=true\ntrap finish EXIT\nexit 17\n' "$backup"
} > "$test_dir/failed-update.sh"
if bash "$test_dir/failed-update.sh"; then
    echo "A failed update returned success" >&2
    exit 1
else
    [[ $? == 17 ]]
fi
cmp "$backup/binary" "$binary"
cmp "$backup/runtime.env" "$runtime_env"
[[ $(sha256sum "$test_dir/live/.env") == "$before" ]]

# A failed restore must stop the rollback before trying to restart the service.
cp() { return 75; }
export -f cp
if bash "$backup/rollback.sh"; then
    echo "Rollback ignored a failed restore" >&2
    exit 1
else
    [[ $? == 75 ]]
fi
echo "Native update rollback and environment checks passed."
