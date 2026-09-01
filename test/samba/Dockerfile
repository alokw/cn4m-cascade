# Minimal Samba server for the integration harness (SPEC.md §12).
# Built from debian rather than dperson/samba so it is architecture-native
# on arm64 hosts and so the container can be killed mid-transfer in Phase 2.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends samba samba-common-bin \
 && rm -rf /var/lib/apt/lists/*

COPY smb.conf /etc/samba/smb.conf
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

EXPOSE 445/tcp
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
