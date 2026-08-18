# Audited public-edge image

This directory defines one Linux/amd64 image for `cmd/djonehub-edge`. Nothing
here deploys it. The final stage is `scratch`: it contains only the static edge
binary, a static loopback health probe, and the CA root bundle copied from the
digest-pinned Go builder. It has no shell, package manager, source tree,
credentials or built-in configuration. Compose may replace its numeric user,
so a runtime owned by any non-zero UID/GID can mount its private 0600 files and
run with a read-only root.

## Provenance requires two commits

`source.lock` intentionally does not hash itself. Never invent or reuse an old
revision merely to make the lock pass. The selected closure includes the Go
application and embedded UI, Dockerfile/health probe, `toolchain.lock`, and the
build/audit/publish/provenance verifiers that determine the artifact:

1. Commit every selected application/UI/image input as commit A. The selected
   paths are defined in `verify-source.py` and include the embedded mobile UI.
2. With those paths clean and `HEAD == A`, generate the lock:

   ```bash
   REVISION=$(git rev-parse HEAD)
   PYTHONDONTWRITEBYTECODE=1 python3 -B \
     deploy/public-edge/image/refresh-source-lock.py \
     --repo-root "$PWD" \
     --output "$PWD/deploy/public-edge/image/source.lock" \
     --revision "$REVISION"
   ```

3. Commit only the resulting `source.lock` (and any documentation that is not a
   selected build input) as commit B. The image label and deterministic build
   timestamp then truthfully identify commit A, while commit B records its
   immutable source-tree digest.

`toolchain.lock` pins Docker 29.1.3, its containerd image store
(`io.containerd.snapshotter.v1`), Compose 2.40.3, the linux/amd64 Go builder
target digest, Dockerfile frontend, Syft and Grype. It is itself part of commit
A's selected source closure, so an uncommitted toolchain change cannot pass
provenance. The build downloads only `golang.org/x/net@v0.50.0` from the fixed
Go proxy; `go.sum` authenticates its content, and compilation runs with
networking disabled.

The exact VPS contract is Docker client/server `29.1.3`, native `linux/amd64`
and image store `io.containerd.snapshotter.v1`. Compose must be semantic version
`2.40.3`, Ubuntu package
`docker-compose-v2=2.40.3+ds1-0ubuntu1~24.04.1`, with the root-owned,
non-writable plugin at `/usr/libexec/docker/cli-plugins/docker-compose` and
SHA-256
`d87a11e944c990dc9f2186115b1136c1cbffffc870845caff0cbdcce0780f41d`.
Buildx must be v0.30.1 at
`/usr/libexec/docker/cli-plugins/docker-buildx`, SHA-256
`c37114fcd034025ec68e224657c8a5a850df472ded3ddcbca75ad3a7ebb9710d`.
The target VPS was observed without that Buildx plugin, so the live build
prerequisite is currently missing. An administrator must acquire and verify the
official Linux/amd64 binary separately, then install it root-owned and not
group/world writable. No live build has passed.

## Hermetic checks

Before commit A (when no truthful lock can exist yet):

```bash
deploy/public-edge/image/test-static.sh --prelock
```

After commit B:

```bash
deploy/public-edge/image/test-static.sh
deploy/public-edge/image/build-image.sh \
  --output /srv/dji4g-public-edge/build-v1 \
  --repository local.invalid/maccellular/djonehub-edge
```

Both commands are dry-run/no-Docker by default. The static suite uses synthetic
Docker inspect, SBOM and CVE evidence and never contacts a daemon or network.

## Live build and audit sequence

Run the following as the non-root deployment owner. The locked builder,
Dockerfile frontend, Syft and Grype images must first be acquired by their exact
digests and independently checked. The first-release live workflow supports
only Docker 29.1.3's containerd image store on native linux/amd64; a classic
image store is rejected before mutation. Every daemon call is forced through
the local `default` context with a new private, empty Docker CLI config, so
ambient or persistent remote contexts and proxy injection cannot redirect the
build. The scripts use direct Docker when allowed, otherwise only
`sudo -n docker`; they never sudo the whole workflow.

Before any live build/audit mutation, the target daemon must return exactly
this ordered `docker info --format '{{json .SecurityOptions}}'` value:

```json
["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]
```

Missing AppArmor, a non-builtin seccomp profile, missing private cgroup
namespace support, any additional entry, or a different order/value fails
closed. This is a live target prerequisite, not evidence that the current VPS
has already passed the production gate.

```bash
deploy/public-edge/image/build-image.sh \
  --output /srv/dji4g-public-edge/build-v1 \
  --repository local.invalid/maccellular/djonehub-edge \
  --buildx-bin /usr/libexec/docker/cli-plugins/docker-buildx \
  --apply
```

When daemon access falls back to existing `sudo -n docker`, the administrator
must also provision `/etc/dji4g-public-edge/docker-cli/config.json` as the exact
bytes `{}\n`, SHA-256
`ca3d163bab055381827226140568f3bef7eaac187cebd76878e0b63e9e442356`.
It must be a single-hard-link root-owned regular file with mode `0644`; its
complete physical path chain must not be group/world writable. The file contains
no credential, proxy, helper or plugin setting. Readability lets the non-root
wrapper verify the exact bytes before sudo; integrity comes from root ownership,
non-writability and the locked hash, not secrecy. The scripts do not create or
repair this administrator-owned prerequisite.

The build runs twice without cache and refuses different Docker image IDs. With
the locked containerd store, `docker image inspect .Id` is the exported OCI
target descriptor digest: it may be the image manifest digest or a bounded
single-platform index digest; it is **not** assumed to be the config-object
digest. The output contains the OCI `docker save` archive, archive SHA-256,
inspect JSON and that exact local `sha256:<64>` target reference. It also records
and retains one unpredictable, build-owned local tag in
`local-image-retention-reference.txt`. Docker 29's containerd store removes the
target when its last local reference is deleted, so this tag must remain until
audit/deployment no longer needs the exact ID; the second build tag is removed.
It is never treated as a RepoDigest or deployment identity.

The verifier binds the top target descriptor through any bounded nested index
to exactly one linux/amd64 manifest, then binds its config and every layer by
raw digest and size. Layer order/count must equal the compatibility manifest;
each layer must be gzip, and its streamed uncompressed SHA-256 must exactly
equal the matching `rootfs.diff_ids` entry. zstd, identity, foreign and unknown
layer media types fail closed because the locked host toolchain has no pinned
zstd decoder. The resulting target/config/layer chain must agree with Docker
inspect, `build.json`, commit revision/created time, source-tree digest and both
lock-file byte digests. A coherently rehashed replacement layer, inspect A plus
archive B, or local tag substituted for a RepoDigest is rejected.

Generate an **edge-only** runtime with that exact ID, then audit it:

```bash
deploy/public-edge/image/audit-image.sh \
  --build-dir /srv/dji4g-public-edge/build-v1 \
  --runtime-root /srv/dji4g-public-edge/runtime-v1 \
  --output /srv/dji4g-public-edge/image-evidence-v1 \
  --apply
```

Before any daemon call, the audit copies `compose.env`, `edge.env`, the gateway
public key and the Access allowlist into a private snapshot, verifies that
snapshot, and thereafter mounts/consumes only those bytes. Their SHA-256 values
are separate evidence fields; the Cloudflare token is used only while verifying
the private snapshot and is then removed without being recorded or hashed.
The audit exports and inspects the scratch rootfs, proves static amd64 ELF
binaries and CA roots, runs the exact image as the generated arbitrary UID with
a read-only root, `runtime=runc`, private IPC/cgroup namespaces,
`AppArmorProfile=docker-default`, `no-new-privileges:true` and
`seccomp=builtin`, waits for the in-image healthcheck, generates SPDX JSON with
digest-pinned Syft, updates the Grype database, then scans offline and rejects
every High/Critical finding. A completed `evidence.json` is valid for at most
48 hours by default. If Docker or a scanner is unavailable, only dry-run/static
checks can pass; there is no live evidence claim.

The verifier requires the startup inspect to contain exactly
`.AppArmorProfile == "docker-default"` and a three-entry
`HostConfig.SecurityOpt` set containing only `no-new-privileges:true`,
`apparmor=docker-default` and `seccomp=builtin`, together with the locked
runtime, namespace, capability, UID:GID, resource, mount, network, log,
restart and health constraints. Docker's real JSON fields
`CpuRealtimePeriod`, `CpuRealtimeRuntime`, `CpuCount` and `CpuPercent` must all
be zero; `Sysctls` may only be omitted or empty. `MaskedPaths` must contain the
fixed Docker 29 base set and may only add canonical per-CPU
`thermal_throttle` paths in numeric CPU order. This `image-evidence` proves the isolated audit
container at collection time. It is not evidence that the production Compose
containers ran on the VPS; only a separately completed and verified
`start-edge` action evidence can establish that production runtime state, and
neither artifact proves real Cloudflare/iOS/Android end-to-end acceptance.
The production action verifier replays archived image evidence with the
recorded action completion time as its `--as-of` boundary; the deployment
preflight still requires the audit to be fresh at the real mutation time. This
preserves later offline verification without turning an old audit into fresh
deployment authorization.

For a private registry, perform the explicit external write only after the
local audit:

```bash
deploy/public-edge/image/publish-image.sh \
  --evidence /srv/dji4g-public-edge/image-evidence-v1 \
  --repository ghcr.io/owner/djonehub-public-edge \
  --tag v1 \
  --registry-auth-dir /secure-input/docker-registry-auth \
  --output /srv/dji4g-public-edge/published-evidence-v1 \
  --confirm-push
```

The explicit auth directory must be a physical current-user-owned `0700`
directory whose `config.json` is `0600` and contains only a non-empty `auths`
object. The script first makes and validates a private snapshot and the push
uses only that snapshot, so later changes to the supplied directory cannot alter
the in-flight authorization. It is used only for the registry push; all daemon routing remains fixed
to the same local `default` context. Ambient Docker auth, contexts, credentials
and proxies are ignored. Publication requires direct non-root access to the
local Docker daemon. If only `sudo -n docker` is available, publication fails
closed; the workflow never exposes the user's registry credentials to root.

The output is `name@sha256:<manifest>` only after Docker reports exactly one
pushed RepoDigest and resolves it to the same audited containerd target ID.
The pushed `${tag}-${random_nonce}` registry tag is a real remote mutation and
is deliberately not auto-deleted. Completed evidence records that tag and its
retention policy. If the push response is lost or the process is interrupted
after the remote write begins, the tag may already exist; reconcile that exact
nonce tag at the registry before retrying and do not call the attempt rolled back.
Without a registry, load the archive on the VPS and use its exact local
`sha256:<target-id>`; only the edge image permits this form. cloudflared and
coturn always require repository digests. Build, audit and publication handle
HUP/INT/TERM as cancellation, clean only their registered temporary paths or
returned container IDs, and do not emit completed evidence after cancellation.

## Static evidence versus live acceptance

`test-static.sh` and all commands without `--apply` use synthetic fixtures or
perform dry-run validation. They do not connect to Docker, build an image,
start a container, update a vulnerability database, publish a registry tag, or
exercise Cloudflare/iOS/Android. A live claim requires the completed build and
audit outputs from the exact locked VPS toolchain; public availability further
requires separate real Cloudflare Access/Tunnel and iOS/Android acceptance.
The embedded system-browser UI source and local tests are complete, but those
real public and device gates have not passed.

## Trust and tamper boundary

The workflow narrows Docker routing to the local `default` context and validates
the locked plugin paths, ownership, writability and hashes. It still trusts the
host Bash/Python/Git/Docker CLI, `PATH`, root administrator, Docker daemon and
principals that can rewrite the repository or evidence parent directory.
Self-hashed manifests detect corruption, missing artifacts and accidental
build/audit/deploy evidence mixing; they are not signatures and do not resist a
malicious same-permission writer that can recompute the hashes. Anchor commit B
and the final evidence digest, or a digital signature, outside that permission
boundary when adversarial tamper resistance is required.
