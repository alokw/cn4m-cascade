#!/usr/bin/env bash
# Blocks until both Samba servers accept TCP on 445. Runs inside the dev
# container, which has no smbclient — a plain connect is enough to know smbd
# has bound the port.
set -uo pipefail

A="${CN4M_TEST_SAMBA_A:-172.28.0.10}"
B="${CN4M_TEST_SAMBA_B:-172.28.0.11}"

for _ in $(seq 1 60); do
    if (exec 3<>"/dev/tcp/$A/445") 2>/dev/null && (exec 3<>"/dev/tcp/$B/445") 2>/dev/null; then
        echo "samba ready ($A, $B)"
        exit 0
    fi
    sleep 1
done

echo "samba never opened port 445 on $A / $B" >&2
exit 1
