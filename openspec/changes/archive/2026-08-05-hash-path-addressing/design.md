## Context

The service today addresses each file as `{uuid}/{filename}` where `filename` is
a single path segment — any `/` is rejected by the sanitizer. External systems,
however, naturally think of files in terms of logical paths
(`财务/2024/年报.docx`). Two pressures motivate this change:

1. Clients want to address files by hierarchical logical paths.
2. We want to decouple the human-readable name from the storage key — for
   normalization, collision-safety, and a degree of name obfuscation — without
   adding a database or changing the versioning model.

The key realization: hashing the logical name to a single storage segment
achieves all of the above *and keeps storage flat* (`{uuid}/{<hash>}{<ext>}`),
while preserving version semantics because the hash is deterministic on the name
(never the content). The existing storage layer (single versioned bucket, native
MinIO Object Versioning) needs no mechanism change — only the handler/sanitizer
translation from logical path to hashed key.

Tech stack is unchanged: Go, Gin, minio-go v7.

## Goals / Non-Goals

**Goals:**
- Let clients address files by a logical path that may contain `/`.
- Derive the storage key deterministically from the logical name via SHA-256,
  preserving the extension, so the same logical path always maps to the same
  object key and continues to accumulate versions.
- Remove the URL-path action ambiguity (`versions` / `rollback` colliding with
  real directory names) by moving actions into an explicit `?action=` query.
- Keep the service stateless and MinIO as the sole source of truth.

**Non-Goals:**
- Server-side directory browsing. Logical directories do not exist in storage;
  `ListFiles` is removed and not replaced.
- Content-addressed storage. The hash is name-based on purpose — content
  addressing would break the versioning model.
- Storing/recovering the original logical name in listings (would require the
  MinIO-private `WithMetadata` extension or N×`HeadObject`; deliberately avoided).
- Any change to the rollback semantic, the bootstrap/versioning logic, or the
  bucket layout mechanism.

## Decisions

### Decision 1: Address by logical path; hash the full name (incl. extension) to a flat key
**Choice:** The client addresses a file by its logical path (may contain `/`).
The server computes `h = sha256(logical_name)` over the raw UTF-8 bytes of the
*entire* logical name **including its extension**, and stores the object at
`{uuid}/{hex(h)}{ext}`, where `ext` is the original extension preserved verbatim.

**Alternatives considered:**
- *Hash the content, not the name:* would make every distinct content a distinct
  key, destroying the versioning model (same logical file, different bytes →
  different objects, no shared history). Rejected.
- *Preserve the directory hierarchy in storage (`{uuid}/a/b/c/file.ext`):* gains
  nothing the client doesn't already track, loses the normalization/obfuscation
  benefits, and complicates listing. Rejected.
- *Hash the name without the extension:* works (the extension is re-attached to
  the key anyway), but hashing the whole string is simpler to reason about and
  keeps the hash input unambiguous.

**Rationale:** Name-based hashing gives a deterministic, flat, storage-safe key
that preserves version semantics and delivers normalization + collision-safety +
obfuscation in one step.

### Decision 2: Full-length SHA-256, hex-encoded; extension preserved on the leaf
**Choice:** Use the full 64-character hex digest (no truncation). Append the
original extension to the stored leaf so content-type detection and the existing
`contentTypeFor` helper keep working unchanged.

**Alternatives considered:**
- *Truncate the digest (e.g. 16 hex chars):* shorter keys at the cost of
  non-negligible collision probability; rejected — S3 key length is not a
  constraint here.

**Rationale:** Zero practical collision concern; the extension on the leaf keeps
MIME resolution identical to today.

### Decision 3: Canonicalization = raw decoded bytes, no normalization
**Choice:** The hash input is the HTTP-decoded logical name as raw UTF-8 bytes,
with **no** Unicode normalization, case-folding, slash collapsing, or trailing-
slash trimming. The canonicalization rule is fixed and documented as a hard
constraint.

**Alternatives considered:**
- *Apply NFC normalization / case-folding:* would require both client and server
  to agree on a canonical form; fragile and a likely source of "file not found"
  bugs. Rejected.

**Rationale:** Upload and download must hash bit-identically. Any silent
normalization on one side but not the other breaks the round-trip. The safest
contract is "hash exactly what was sent."

### Decision 4: Actions move to an explicit `?action=` query parameter
**Choice:** The `*filepath` catch-all is treated wholesale as the logical name
(leading `/` trimmed). Operations beyond plain upload / download / delete are
selected by an explicit query parameter:

| Method | Query | Action |
| --- | --- | --- |
| `POST` | *(none)* | upload |
| `POST` | `?action=rollback` | rollback (target version via JSON body or `?versionId=`) |
| `GET` | *(none)* | download latest |
| `GET` | `?action=versions` | list version history |
| `GET` | `?action=version&versionId=<vid>` | download a specific version |
| `DELETE` | *(none)* | delete file + all versions |

**Alternatives considered:**
- *Keep action suffixes in the path with a "reserved name" convention:* leaves a
  latent bug where a real directory named `versions` or `rollback` is
  mis-parsed. Rejected.
- *URL-encode the logical name as a single `%2F`-escaped segment:* avoids the
  ambiguity without a route change, but Gin/Go's handling of `%2F` in catch-all
  params is a known gotcha (`UseRawPath` / `UnescapePathValues`) and would need a
  spike. Rejected in favor of the clearer query-based design.

**Rationale:** Real slashes in the catch-all are Gin's natural fit; moving
actions to the query removes all parsing ambiguity and yields a self-documenting,
extensible API.

### Decision 5: Remove `ListFiles`; do not recover original names
**Choice:** Drop the `GET /documents/:uuid` endpoint. Do not attempt to return
the original logical name in any listing.

**Alternatives considered:**
- *Keep `ListFiles` returning hashes:* useless to clients (hashes are not
  reversible) and misleading. Rejected.
- *Recover original names via minio-go `WithMetadata: true`:* relies on a
  MinIO-private extension (`ListObjectsV2WithMetadata`) with a history of decode
  bugs, and narrows S3 compatibility. Rejected.
- *Recover names via `HeadObject` per file:* N extra round-trips for a feature
  with no identified consumer. Rejected.

**Rationale:** Logical directory structure is a client-side concept; the server
has no need to understand it. Removing the endpoint keeps the service stateless,
S3-portable, and simple. `ListDirectories` (`GET /documents`) is unaffected — it
still enumerates top-level `{uuid}/` prefixes.

### Decision 6: Logical-name validation rules
**Choice:** Validate the logical name as a safe path before hashing:
- Non-empty; no leading/trailing `/`; no empty segments (`a//b` rejected).
- No `.` or `..` segments (path traversal is structurally impossible post-hash,
  but these are rejected as almost-certain client bugs / to enforce a single
  canonical form, consistent with Decision 3).
- No NUL or C0 control characters.
- Enforce a total length cap (e.g. 1024 bytes) for hygiene.

**Rationale:** Because hashing neutralizes any actual escape risk, validation
here is about input hygiene and guaranteeing a single canonical form per logical
name, which protects the round-trip guarantee.

## Risks / Trade-offs

- **[Breaking API change]** → README, examples, and all clients must update;
  documented as BREAKING in the proposal and tasks.
- **[Old data unreachable]** Existing objects written under the verbatim
  `{uuid}/{filename}` key shape cannot be found via the new API (a verbatim
  filename no longer hash-matches its old key). → See Migration Plan; for the
  greenfield MVP this is a non-issue.
- **[Canonicalization fragility]** Any divergence in decoding/normalization
  between upload and download yields a "not found". → Mitigated by Decision 3
  (fixed, documented rule) and covered by a dedicated round-trip test.
- **[Loss of server-side browsing]** Operators can no longer list a UUID's
  contents readably. → Accepted; clients track their own logical paths. Raw
  `ListObjects` against the bucket still works for ops/debugging if ever needed.
- **[Hash is opaque]** Stored keys are not human-meaningful. → Accepted; this is
  an intended consequence (obfuscation), and the extension is preserved for MIME
  purposes.

## Migration Plan

- **If no existing data** (expected for this greenfield MVP): deploy the new
  build; no data action. Rollback = redeploy the previous image; MinIO data is
  untouched by a service rollback.
- **If existing data must be preserved:** run a one-time re-key script that, for
  each old object `{uuid}/{filename}`, treats the old single-segment filename as
  the logical name, computes the new key `{uuid}/{sha256(filename)}{ext}`, and
  performs a server-side `CopyObject` (preserving versions) followed by removal
  of the old key. Whether this is needed is an Open Question pending confirmation
  that no production data exists.

## Open Questions

- Is there any stored data under the old key shape that requires migration?
  (Assumption: none — MVP, not yet in production.)
- Final total length cap for the logical name (1024 bytes proposed)?
- Should the re-key script be implemented now or only if data is found?
