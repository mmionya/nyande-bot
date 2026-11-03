#!/usr/bin/env bash
# Update the existing root-owned systemd installation without editing its .env.
set -Eeuo pipefail

restore_file() {
    local saved=$1 target=$2
    if [[ -e "$saved" ]]; then
        cp -a -- "$saved" "$target.nyande-restore"
        mv -f -- "$target.nyande-restore" "$target"
    elif [[ -e "$target" ]]; then
        rm -- "$target"
    fi
}

restore_release() {
    restore_file "$backup/binary" "$binary"
    restore_file "$backup/drop-in" "$drop_in"
    restore_file "$backup/runtime.env" "$runtime_env"
    systemctl daemon-reload
    systemctl restart "$unit"
}

write_rollback_script() {
    printf '#!/usr/bin/env bash\nset -euo pipefail\n'
    printf 'backup=%q\nbinary=%q\ndrop_in=%q\nruntime_env=%q\nunit=%q\n' \
        "$backup" "$binary" "$drop_in" "$runtime_env" "$unit"
    declare -f restore_file restore_release
    printf 'restore_release\n'
}

finish() {
    local status=$?
    trap - EXIT
    if (( status != 0 )) && [[ ${activating:-false} == true ]]; then
        echo "Update failed; restoring the previous binary and systemd settings." >&2
        # A separate shell with errexit must stop if any restoration step fails.
        if bash "$backup/rollback.sh"; then
            echo "Previous version restored." >&2
        else
            echo "Automatic rollback failed. Backup: $backup" >&2
        fi
    fi
    if [[ -n ${work:-} && -d "$work" ]]; then
        rm -r -- "$work"
    fi
    exit "$status"
}

write_runtime_env() {
    local venv=$1
    # EnvironmentFile quoting: reject unusual paths instead of interpreting them.
    [[ "$venv" != *$'\n'* && "$venv" != *$'\r'* &&
       "$venv" != *'"'* && "$venv" != *\\* ]] || return 1
    printf 'WHISPER_ENABLED=false\nSTICKER_PYTHON_PATH="%s/bin/python3"\n' "$venv"
}

main() {
    local revision=${1:-main}
    [[ $# -le 1 && "$revision" =~ ^(main|[a-f0-9]{40})$ ]] || {
        echo "Usage: bash update-systemd.sh [main|commit-sha]" >&2; return 1;
    }
    [[ $EUID -eq 0 ]] || { echo "Run as root." >&2; return 1; }
    command -v apt-get >/dev/null || {
        echo "This updater requires Debian or Ubuntu (apt-get)." >&2; return 1;
    }
    [[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || {
        echo "This updater targets Linux x86_64." >&2; return 1;
    }
    unit=nyande-bot.service
    binary=/usr/local/bin/nyande-bot
    drop_in=/etc/systemd/system/$unit.d/99-nyande-native-update.conf
    runtime_env=/etc/nyande-bot-native/runtime.env
    local app_dir service_user pid env_file env_hash available_kb
    service_user=$(systemctl show "$unit" -p User --value)
    [[ -z "$service_user" || "$service_user" == root ]] || {
        echo "Expected the existing root-owned service; refusing to change another setup." >&2
        return 1
    }
    app_dir=$(systemctl show "$unit" -p WorkingDirectory --value)
    [[ "$app_dir" == /root/nyande-bot ]] || {
        echo "Expected WorkingDirectory=/root/nyande-bot." >&2; return 1;
    }
    env_file=$app_dir/.env
    [[ -f "$env_file" && -f "$binary" && ! -L "$binary" ]] || {
        echo "Existing .env and regular installed binary are required." >&2; return 1;
    }
    systemctl is-active --quiet "$unit"
    pid=$(systemctl show "$unit" -p MainPID --value)
    [[ $(readlink -f "/proc/$pid/exe") == "$binary" ]] || {
        echo "The service is running a different executable; refusing to replace it." >&2
        return 1
    }
    exec 9>/run/lock/nyande-bot-update.lock
    flock -n 9 || { echo "Another update is running." >&2; return 1; }
    env_hash=$(sha256sum "$env_file")
    available_kb=$(df -Pk /var/tmp | awk 'END {print $4}')
    (( available_kb >= 2 * 1024 * 1024 )) || {
        echo "At least 2 GiB of free space in /var/tmp is needed for the build." >&2
        return 1
    }
    umask 077
    work=$(mktemp -d /var/tmp/nyande-update.XXXXXXXX)
    activating=false
    trap finish EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 143' TERM

    echo "Installing PDF/sticker tools. Whisper and PyTorch are not installed."
    apt-get update
    DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l apt-get install -y --no-install-recommends \
        ca-certificates curl git ffmpeg poppler-utils python3-venv

    echo "Fetching the requested source into a temporary checkout."
    git init -q "$work/source"
    git -C "$work/source" remote add origin https://github.com/mmionya/nyande-bot.git
    GIT_TERMINAL_PROMPT=0 git -C "$work/source" fetch -q --depth=1 origin "$revision"
    git -C "$work/source" checkout -q --detach FETCH_HEAD
    local commit go_version go_bin filename checksum
    commit=$(git -C "$work/source" rev-parse HEAD)
    go_version=$(awk '$1 == "go" {print $2; exit}' "$work/source/go.mod")
    [[ "$go_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
    # Verify the toolchain archive against the official HTTPS release manifest.
    curl --fail --silent --show-error --location --retry 3 \
        'https://go.dev/dl/?mode=json&include=all' -o "$work/go-releases.json"
    filename=go$go_version.linux-amd64.tar.gz
    checksum=$(python3 - "$work/go-releases.json" "$filename" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    files = [item for release in json.load(f) for item in release["files"]]
matches = [item["sha256"] for item in files if item["filename"] == sys.argv[2]]
if len(matches) != 1:
    raise SystemExit("Required Go release is missing from the official manifest")
print(matches[0])
PY
    )
    curl --fail --silent --show-error --location --retry 3 \
        "https://go.dev/dl/$filename" -o "$work/go.tar.gz"
    printf '%s  %s\n' "$checksum" "$work/go.tar.gz" | sha256sum --check --status
    tar -xzf "$work/go.tar.gz" -C "$work"
    go_bin=$work/go/bin/go
    echo "Building $commit with one worker; the existing bot keeps running."
    (
        cd "$work/source"
        CGO_ENABLED=0 GOTOOLCHAIN=local GOMAXPROCS=1 GOGC=30 GOMEMLIMIT=256MiB \
            GOCACHE="$work/go-cache" GOMODCACHE="$work/go-modules" TMPDIR="$work" \
            "$go_bin" build -p 1 -trimpath -ldflags='-s -w' -o "$work/nyande-bot" ./cmd/nyande-bot
    )
    local venv
    mkdir -p /opt/nyande-bot-media
    venv=$(mktemp -d /opt/nyande-bot-media/runtime.XXXXXXXX)
    python3 -m venv "$venv"
    "$venv/bin/python3" -m pip install --no-cache-dir 'rlottie-python[full]==1.3.8'
    gzip -dc "$work/source/internal/bot/testdata/sticker.tgs" > "$work/sticker.json"
    "$venv/bin/python3" "$work/source/internal/bot/render_tgs.py" "$work/sticker.json" "$work" 1
    [[ -s "$work/frame-00.png" ]]
    local dependency
    for dependency in ffmpeg pdftotext pdfinfo pdftoppm; do
        command -v "$dependency" >/dev/null
    done
    write_runtime_env "$venv" > "$work/runtime.env"
    printf '[Service]\nEnvironmentFile=%s\n' "$runtime_env" > "$work/drop-in"
    [[ $(sha256sum "$env_file") == "$env_hash" ]] || {
        echo ".env changed during preparation; leaving the running deployment alone." >&2
        return 1
    }

    mkdir -p /var/backups/nyande-bot "$(dirname "$drop_in")" "$(dirname "$runtime_env")"
    backup=$(mktemp -d /var/backups/nyande-bot/update.XXXXXXXX)
    cp -a -- "$binary" "$backup/binary"
    [[ ! -e "$drop_in" ]] || cp -a -- "$drop_in" "$backup/drop-in"
    [[ ! -e "$runtime_env" ]] || cp -a -- "$runtime_env" "$backup/runtime.env"
    printf '%s\n' "$commit" > "$backup/new-commit"
    write_rollback_script > "$backup/rollback.sh"
    echo "Backup: $backup"
    activating=true
    install -m 0600 "$work/runtime.env" "$runtime_env.nyande-new"
    mv -f -- "$runtime_env.nyande-new" "$runtime_env"
    install -m 0644 "$work/drop-in" "$drop_in.nyande-new"
    mv -f -- "$drop_in.nyande-new" "$drop_in"
    install -m 0755 "$work/nyande-bot" "$binary.nyande-new"
    mv -f -- "$binary.nyande-new" "$binary"
    systemctl daemon-reload
    systemctl restart "$unit"
    pid=$(systemctl show "$unit" -p MainPID --value)
    [[ "$pid" =~ ^[1-9][0-9]*$ ]]
    echo "Checking that the new process remains running for 30 seconds."
    local attempt
    for ((attempt=0; attempt<15; attempt++)); do
        sleep 2
        systemctl is-active --quiet "$unit"
        [[ $(systemctl show "$unit" -p MainPID --value) == "$pid" ]]
    done
    python3 - "$pid" "$venv" <<'PY'
import pathlib, sys
values = dict(item.split(b"=", 1) for item in pathlib.Path(
    "/proc/" + sys.argv[1] + "/environ").read_bytes().split(b"\0") if b"=" in item)
if values.get(b"WHISPER_ENABLED") != b"false":
    raise SystemExit("Another service setting overrode WHISPER_ENABLED=false")
if values.get(b"STICKER_PYTHON_PATH") != (sys.argv[2] + "/bin/python3").encode():
    raise SystemExit("Another service setting overrode STICKER_PYTHON_PATH")
PY
    [[ $(sha256sum "$env_file") == "$env_hash" ]]
    activating=false
    echo "Updated to $commit. Existing .env unchanged; Whisper disabled."
    systemctl show "$unit" -p ActiveState -p SubState -p MainPID --no-pager
    printf 'Rollback command: bash %q\n' "$backup/rollback.sh"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
    main "$@"
fi
