# Public relay v2 core contract

This directory is a cryptographic contract and test oracle, not a runnable
relay. It contains no network listener, modem access, pairing UI, key storage,
durable state implementation, or durable SMS journal. In particular, nothing
in this package establishes end-to-end availability or safe production SMS
execution by itself.

All text is UTF-8. `LP(x)` means a four-byte unsigned big-endian byte length
followed by the bytes of `x`. Decimal integers have no sign or leading zero.
Binary wire values use unpadded RFC 4648 base64url. Route, message, and
operation IDs decode to exactly 16 non-zero bytes.

JSON inputs are bounded and must have the exact case-sensitive schema. Unknown,
missing, or duplicate keys, trailing values, excessive nesting, invalid raw
UTF-8, isolated UTF-16 surrogate escapes, non-canonical base64url, and `null`
where an array is required are rejected. Legal UTF-16 surrogate pairs are
accepted. Public parsing errors are fixed short values and do not echo attacker
controlled keys, kinds, directions, or values.

## Key schedule

`Kroot` is exactly 32 non-zero bytes supplied by a separately authenticated
local pairing protocol. For one route and key epoch:

1. `salt_input = LP("DJONEHUB-PUBLIC-RELAY-KDF-SALT-V2") ||
   LP("route_id") || LP(route_id) || LP("key_epoch") || LP(epoch_decimal)`.
2. `salt = SHA-256(salt_input)`.
3. `PRK = HKDF-Extract-SHA256(salt, Kroot)`.
4. Expand 32 bytes for each literal info label:
   `DJONEHUB-PUBLIC-RELAY-C2G-AEAD-V2`,
   `DJONEHUB-PUBLIC-RELAY-G2C-AEAD-V2`, and
   `DJONEHUB-PUBLIC-RELAY-BIND-V2`.

The two AES-256-GCM traffic keys and the HMAC binding/state key are independent.

## Outer envelope, AAD, and nonce

The exact required JSON keys are, in encoder order:

`version`, `route_id`, `key_epoch`, `direction`, `sequence`, `message_id`,
`issued_at`, `ciphertext`, `signature`.

`version` is 2. Direction is `c2g` or `g2c`. Epoch, sequence, and positive Unix
seconds are at most `2^53-1`. The outer object never carries an action,
operation ID, phone number, SMS body, cursor, or business result.

The AES-GCM associated data is:

`LP("DJONEHUB-PUBLIC-RELAY-AES-GCM-AAD-V2")` followed by `LP(name) ||
LP(value)` for `version`, `route_id`, `key_epoch`, `direction`, `sequence`,
`message_id`, and `issued_at`, in that order.

For the independently derived `K_direction`, the nonce prefix is:

`prefix = first4(HMAC-SHA256(K_direction,
LP("DJONEHUB-PUBLIC-RELAY-AES-GCM-NONCE-PREFIX-V2") ||
LP("route_id") || LP(route_id) || LP("key_epoch") || LP(epoch_decimal) ||
LP("direction") || LP(direction)))`.

The 12-byte AES-GCM nonce is exactly:

`prefix || uint64be(sequence)`.

The sender must use a durable monotonic allocator that never reuses a sequence
under the same route, epoch, and direction key. Changing `message_id` or
`issued_at` does not change the nonce and cannot make sequence reuse safe.

The receiver checks `message_id` uniqueness only against entries still retained
inside its bounded replay window. After an entry ages out, `message_id` is not a
global idempotency key. Business idempotency for `sms.send` therefore belongs to
the separate durable operation journal keyed by the operation ID and commitment.

`issued_at` is authenticated but this core applies no clock-skew, freshness, or
expiry policy, and version 2 has no `expires_at` field. A production session
layer must authenticate the envelope, then enforce an explicit TTL/skew policy
before any business execution. This package intentionally does not invent that
production policy.

## Inner messages and padding

The decrypted JSON object has exactly `version`, `kind`, and `body`. Allowed
kinds are:

- `sms.send`: body keys `operation_id`, `destination`, `message`.
- `sms.inspect_and_fence`: body keys `operation_id`, `commitment`.
- `sms.inspect_and_fence.result`: body keys `operation_id`, `commitment`,
  `outcome`.

There is deliberately no `not_found` outcome. The allowed result outcomes are
`pending`, `applied`, `failed`, `unknown`, and `fenced_absent`.

Before encryption, the inner JSON is wrapped as:

`"DJR2" || uint32be(json_length) || json || zero_padding`.

The total padded size must be the smallest of 256, 512, 1024, 2048, 4096, 8192,
16384, or 32768 bytes that fits. A larger bucket, even with all-zero padding, is
non-canonical and rejected. AES-256-GCM adds its 16-byte tag.

## SMS operation commitment

`commitment = HMAC-SHA256(K_bind, canonical_send)`, where `canonical_send` is:

`LP("DJONEHUB-PUBLIC-RELAY-SMS-COMMITMENT-V2")` followed by `LP(name) ||
LP(value)` for `version=2`, `kind=sms.send`, `operation_id`, `destination`, and
`message`, in that order. No destination or message normalization occurs.

## P-256 proof and verifier roles

The device signs only `c2g`; the gateway signs only `g2c`. The transcript is:

`LP("DJONEHUB-PUBLIC-RELAY-P256-PROOF-V2")` followed by `LP(name) || LP(value)`
for `version`, `role`, `route_id`, `key_epoch`, `direction`, `sequence`,
`message_id`, `issued_at`, `ciphertext_length`, and `ciphertext_sha256`, in that
order. The ciphertext hash is base64url SHA-256. The proof is ECDSA P-256 over
SHA-256 of this transcript, encoded as canonical ASN.1 DER, normalized to low-S,
then base64url encoded.

The public signing surface accepts only a typed allowlisted `Message` and
performs padding, encryption, and role-specific signing atomically. Proof
digests and signing arbitrary bytes or caller-supplied wire envelopes are not
public APIs.

A c2g receiver pins a deep copy of one exact P-256 device verifier; a g2c
receiver pins a deep copy of one exact P-256 gateway verifier. Callers cannot
supply a verifier to an individual open operation. The pinned verifier's SPKI
SHA-256 fingerprint is included in authenticated receiver state.

## Verified fencing predicate

Raw `SMSInspectAndFenceResult` values expose no success predicate. Only a
non-zero `VerifiedGatewayMessage`, created after pinned gateway proof, AEAD,
schema, replay, and durable-state gates succeed, can evaluate
`ConfirmsFencedAbsent` against a receiver-created expectation. The comparison
requires the exact route, epoch, pinned verifier fingerprint, derived key
context, result kind, operation ID, commitment, direction, and
`fenced_absent` outcome.

Producing a signed result still requires the later gateway integration to hold
the shared operation/send lock and durably append and fsync a permanent
`fenced_absent` tombstone before sealing the result. This package defines that
contract but cannot attest that an external storage or modem integration obeyed
it.

## Replay state and durable acceptance

The receiver permits bounded out-of-order delivery. An exact authenticated
duplicate returns `exact_duplicate` and must never re-execute a mutation; the
caller reconciles it against the durable operation journal. Reusing a retained
sequence or message ID with different content is rejected.

For every new message, one receiver-local lock covers classification, AEAD open,
candidate state creation, and the commit call. The mandatory committer receives
the expected prior generation and state ID plus the next authenticated bytes.
A successful implementation must compare-and-swap both expected values,
durably replace the state, perform the platform's fsync-equivalent barrier, and
only then return success. The receiver updates memory and releases verified
plaintext only after that success. CAS failure, pre-write failure, and ambiguous
post-write failure release no verified plaintext.

CAS prevents concurrent stale writes only while the authoritative store keeps
and atomically compares its latest generation and state ID. It cannot detect an
offline rollback that restores both the authenticated state bytes and the CAS
metadata to an older, mutually consistent pair; production rollback resistance
must be supplied outside this package.

This package supplies only the committer interface and authenticated serialized
state model. It does not implement files, databases, atomic replacement, CAS,
or fsync. State authentication covers route, epoch, direction, pinned verifier
fingerprint, replay window, high-water mark, generation, previous/current state
IDs, and retained replay entries.

## Golden vectors

`testdata/golden_vectors.json` is synthetic. It includes KDF salt input, salt,
PRK, derived keys, nonce prefixes, full nonces, padding, AAD, ciphertext, proof
digests, fixed valid signatures, SEC1 and SPKI public-key encodings, SPKI
fingerprints, strict Unicode cases, and final envelopes for later Java alignment.
