## ADDED Requirements

### Requirement: Files are addressed by logical path and hashed to a storage key
The system SHALL address each file by a logical path supplied by the client, where the logical path MAY contain `/`-separated segments. The system SHALL derive the storage object key by computing SHA-256 over the raw UTF-8 bytes of the entire logical name — including its file extension — encoding the digest as a lowercase hex string, and storing the object at `{uuid}/{<sha256-hex>}{<ext>}`, where `<ext>` is the original extension preserved verbatim. The hash SHALL be deterministic on the logical name only (never on file content), so that repeated operations on the same logical path address the same object and its version history. The system SHALL apply no Unicode normalization, case-folding, or slash collapsing to the logical name before hashing — the exact decoded bytes supplied by the client SHALL be hashed. The system SHALL reject logical names that are empty, contain empty segments, contain `.` or `..` segments, or contain NUL or C0 control characters.

#### Scenario: Logical path with directories hashes to a flat key
- **WHEN** a client addresses the logical path `财务/2024/年报.docx` under UUID `abc`
- **THEN** the system stores the object at `{abc}/{sha256("财务/2024/年报.docx")}.docx`, with no directory structure reflected in the storage key

#### Scenario: Same logical path always maps to the same key
- **WHEN** a client addresses the same logical path twice under the same UUID
- **THEN** both operations resolve to the same storage object key, so the second operation acts on the version history of the first

#### Scenario: Different logical paths never collide
- **WHEN** two distinct logical paths are addressed under the same UUID
- **THEN** they map to distinct storage object keys

#### Scenario: Extension is preserved on the stored key
- **WHEN** a client addresses a logical path whose final segment is `report.docx`
- **THEN** the stored object key ends in `.docx`, enabling content-type resolution from the extension

#### Scenario: Same base name, different extensions do not collide
- **WHEN** a client addresses `report.docx` and `report.xlsx` under the same UUID
- **THEN** the two map to distinct storage object keys (because the extension is part of the hash input)

#### Scenario: Malformed logical name is rejected
- **WHEN** a client addresses a logical name that is empty, contains an empty segment (`a//b`), a `.` or `..` segment, or a control character
- **THEN** the system responds with a client error and performs no storage operation

#### Scenario: No silent normalization
- **WHEN** a client addresses a logical path and later addresses a byte-different variant of it (e.g. a different Unicode normalization, or a trailing slash)
- **THEN** the two map to different storage object keys, because no normalization is applied

## MODIFIED Requirements

### Requirement: Upload a file as a new version
The system SHALL accept a file upload addressed by a logical path under a UUID and store it so that MinIO generates a new object version at the path's derived storage key, preserving all prior versions. The system SHALL return the new version identifier and timestamp.

#### Scenario: First upload of a logical path
- **WHEN** a client uploads content addressed to the logical path `report.docx` under UUID `abc` and no object at its derived key exists yet
- **THEN** the system stores the file at the derived key, returns the new `versionId`, and the file becomes the latest version

#### Scenario: Subsequent upload creates a new version
- **WHEN** a client uploads content addressed to the same logical path under UUID `abc` again
- **THEN** the system stores a new version at the same derived key, the previous content is retained as a prior version, and the new upload becomes the latest version

#### Scenario: Same logical path across UUIDs is isolated
- **WHEN** two different UUIDs each address the same logical path
- **THEN** the two files have independent version histories and do not interfere with each other

### Requirement: List version history of a file
The system SHALL return the version history of the object at a logical path's derived key, ordered newest-first, where each entry includes the version identifier, last-modified timestamp, size in bytes, and whether it is the latest version.

#### Scenario: Logical path with multiple versions
- **WHEN** a client requests the history of a logical path that has been uploaded three times
- **THEN** the system returns three version entries ordered newest-first, with exactly one entry marked as latest

#### Scenario: Logical path has never been uploaded
- **WHEN** a client requests the history of a logical path that has never been uploaded
- **THEN** the system responds with a not-found error

### Requirement: Download the latest version
The system SHALL return the content of the latest version of the object at a logical path's derived key.

#### Scenario: Download latest
- **WHEN** a client requests the latest version of a logical path that has been uploaded
- **THEN** the system streams the content of the current latest version with an appropriate content type derived from the extension

#### Scenario: Latest version of a non-existent logical path
- **WHEN** a client requests the latest version of a logical path that has never been uploaded
- **THEN** the system responds with a not-found error

### Requirement: Download a specific historical version
The system SHALL return the content of a specific version of the object at a logical path's derived key, identified by its version identifier.

#### Scenario: Download a historical version
- **WHEN** a client requests a specific version identifier of a logical path
- **THEN** the system streams the exact content of that historical version, even if newer versions exist

#### Scenario: Unknown version identifier
- **WHEN** a client requests a version identifier that does not exist for the logical path
- **THEN** the system responds with a not-found error

### Requirement: Delete a file and all its versions
The system SHALL delete the object at a logical path's derived key including every historical version of that object.

#### Scenario: Delete a versioned logical path
- **WHEN** a client deletes a logical path that has three versions
- **THEN** the system removes the object and all of its versions, and subsequent history/download requests for that logical path respond with not-found

## REMOVED Requirements

### Requirement: List files in a UUID directory
**Reason:** Logical directories no longer exist in storage — logical paths are hashed into flat `{uuid}/{<hash>}{ext}` keys, so there is no directory structure to enumerate. Recovering original logical names in a listing would require the MinIO-private `WithMetadata` listing extension or an extra `HeadObject` per object, neither of which is justified for an MVP with no identified consumer of server-side browsing.
**Migration:** Clients track their own logical paths. `GET /documents` (list UUID directories) remains available; only per-UUID file listing is removed.
