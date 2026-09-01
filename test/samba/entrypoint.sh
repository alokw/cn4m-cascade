#!/bin/sh
# Seed two shares with fixture files, then run smbd in the foreground.
set -e

mkdir -p /srv/public /srv/private /var/log/samba

seed() {
    root="$1"
    [ -e "$root/.seeded" ] && return 0
    mkdir -p "$root/nested/deeper"
    printf 'hello from %s\n' "$(hostname)" > "$root/hello.txt"
    printf '' > "$root/empty.bin"
    printf 'nested payload\n' > "$root/nested/payload.txt"
    printf 'deep payload\n' > "$root/nested/deeper/deep.txt"
    # unicode + emoji names (SPEC.md §12)
    printf 'unicode ok\n' > "$root/ünïcodé-📁.txt"
    touch "$root/.seeded"
}

seed /srv/public
seed /srv/private

# SMB accounts. Two users on the same host so the "same share, different
# credentials" case (PROGRESS.md R-2) is testable.
for u in syncuser otheruser; do
    id "$u" >/dev/null 2>&1 || useradd -M -s /usr/sbin/nologin "$u"
done
printf 'syncpass\nsyncpass\n' | smbpasswd -s -a syncuser >/dev/null
printf 'otherpass\notherpass\n' | smbpasswd -s -a otheruser >/dev/null

exec smbd --foreground --no-process-group --debug-stdout
