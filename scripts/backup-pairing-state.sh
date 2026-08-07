#!/bin/bash
# Backup the state that makes the no-USB refresh path survivable across a
# VPS rebuild. Losing any of these forces a USB re-pair / re-auth session:
#
#   ~/.pymobiledevice3 + /root/.pymobiledevice3   RemotePairing (RPPairing)
#                                                 records - the wireless
#                                                 tunnel only works with them
#   /var/lib/usbmuxd                             USB pair records (if the
#                                                 host has any)
#   /var/lib/sidey/isideload                     Apple cert identity +
#                                                 private key per account.
#                                                 Losing it forces a NEW cert
#                                                 name, which the free team's
#                                                 3/3 quota rejects.
#   /var/lib/sidey/refresh-agent                 agent API key (re-enrolable;
#                                                 cheap to include)
#
# The backup is gpg-symmetric encrypted (AES-256); the passphrase lives
# root-only at /root/.sidey-backup-passphrase. The resulting tarball is
# self-contained and can be copied off-box for real off-host redundancy.
#
# Retention: the newest RETENTION backups are kept.
set -euo pipefail

DEST=/var/backups/sidey
RETENTION="${SIDEY_BACKUP_RETENTION:-7}"
PASSPHRASE_FILE=/root/.sidey-backup-passphrase

SOURCES=(
    /root/.pymobiledevice3
    /home/ubuntu/.pymobiledevice3
    /var/lib/usbmuxd
    /var/lib/sidey/isideload
    /var/lib/sidey/refresh-agent
)

mkdir -p "$DEST"

# Serialise concurrent runs (timer + manual invocation).
exec 9>"$DEST/.lock"
flock -n 9 || { echo "backup already running" >&2; exit 1; }

# Passphrase: generate once, root-only. It is per-host state; copy it along
# with the backup if the whole disk is being migrated.
if [ ! -s "$PASSPHRASE_FILE" ]; then
    umask 077
    head -c 32 /dev/urandom | base64 > "$PASSPHRASE_FILE"
    chmod 600 "$PASSPHRASE_FILE"
fi

STAMP=$(date +%Y%m%d-%H%M%S)
OUT="$DEST/pairing-state-$STAMP.tar.gz.gpg"
TMP="$OUT.tmp"

# Collect only what exists; a missing path is a warning, not a failure (e.g.
# /var/lib/usbmuxd is empty on this host).
TAR_ARGS=(-czf - --warning=no-file-changed)
for src in "${SOURCES[@]}"; do
    [ -e "$src" ] && TAR_ARGS+=("$src")
done
if [ ${#TAR_ARGS[@]} -le 4 ]; then
    echo "nothing to back up (no source paths present)" >&2
    exit 1
fi

# Encrypt directly to the final name; gpg reads the tarball from stdin.
tar "${TAR_ARGS[@]}" | gpg --batch --yes --symmetric --cipher-algo AES256 \
    --passphrase-file "$PASSPHRASE_FILE" --output "$TMP" \
    || { rm -f "$TMP"; echo "encryption failed" >&2; exit 1; }

# Verify: decrypt and confirm every existing source path made it in.
if ! gpg --batch --decrypt --passphrase-file "$PASSPHRASE_FILE" "$TMP" \
        | tar tzf - > /dev/null 2>&1; then
    rm -f "$TMP"
    echo "backup verification failed (corrupt archive)" >&2
    exit 1
fi

mv "$TMP" "$OUT"

# Retention: keep the newest RETENTION archives.
ls -1t "$DEST"/pairing-state-*.tar.gz.gpg 2>/dev/null \
    | tail -n +$((RETENTION + 1)) \
    | while read -r old; do rm -f "$old"; done

echo "pairing state backed up: $OUT ($(du -h "$OUT" | cut -f1))"
