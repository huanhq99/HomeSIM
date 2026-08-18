# Public TURN live probe

`integration/publicturn` is a narrow UDP or TLS/TCP acceptance probe for the
deployed `turn.example.com` service. It issues one two-minute coturn REST
credential via `internal/turnauth`, performs a real Pion TURN v5 allocation,
verifies the relay port is `49160-49167`, and exchanges two different random
datagrams through an independent local UDP peer in both directions. TLS mode
connects on TCP 443, verifies the public certificate with the exact ServerName
`turn.example.com`, and keeps the TURN realm at that same name even when the
TCP destination is pinned to `PUBLIC_TURN_IPV4`.

The peer permission uses the public client IP carried by the authenticated
Allocate success response. The peer's real mapped port is then learned from its
first relayed datagram, so the probe does not depend on anonymous STUN being
available when coturn enables `secure-stun`.

The command is fail-closed unless `MACCELLULAR_PUBLIC_TURN_E2E=1` and an absolute
mode-0600 secret-file path (or `--secret-stdin`) are both present. Secret
content is read from the file or stdin, never placed in argv, and neither
credentials nor payloads are logged. For a remote secret, use an encrypted SSH
pipe so the value is not written to the Mac:

```sh
set -o pipefail
ssh -o BatchMode=yes -o ConnectTimeout=8 turn-vps \
  'sudo -n /usr/bin/cat /srv/dji4g-public-turn/runtime-20260815-1/secrets/turn-auth-secret' |
  MACCELLULAR_PUBLIC_TURN_E2E=1 scripts/tests/public-turn-e2e.sh \
    --secret-stdin --connect-address PUBLIC_TURN_IPV4 --transport udp
```
An optional `--connect-address` pins only the destination IP to bypass a local
fake-DNS resolver; credential host, TLS ServerName, and authentication realm
remain exactly `turn.example.com`. Select TLS/TCP 443 explicitly with
`--transport tls`; the default live transport remains UDP 3478.
Repeatable `--stun-control host:port` inputs provide explicit live, Binding-only
UDP controls without a TURN secret. Each result reports only packet counts and
whether XOR-MAPPED-ADDRESS was verified; the mapped address itself is never
printed.
Use `scripts/tests/public-turn-e2e.sh`; do not invoke the Go command directly in
automation.
