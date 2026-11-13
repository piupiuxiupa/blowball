# version-rollback Specification

## Purpose

Non-destructively restore a prior version of a file as the new latest version,
without discarding any history. Rollback is performed as a server-side copy from
the target version to the same key, creating a new latest version whose content
equals the target; every existing version (including those newer than the target)
is retained and remains retrievable, so a rollback is itself undoable.

## Requirements

### Requirement: Non-destructive rollback to a prior version
The system SHALL roll back `{uuid}/{filename}` to a target version by promoting that version's content to become a new latest version, performed via a server-side copy from the target version. The system SHALL NOT delete any existing versions as part of a rollback — every prior version, including those newer than the target, SHALL remain retrievable afterward.

#### Scenario: Rollback creates a new latest version
- **WHEN** a file `{uuid}/report.docx` has versions `v1, v2, v3(=latest)` and a client requests rollback to `v1`
- **THEN** the system creates a new latest version whose content equals `v1`, `v1`, `v2`, and `v3` all remain in the history, and the rollback result is itself recorded as a new version

#### Scenario: Rollback target is already the latest version
- **WHEN** a client requests rollback to a version that is already the latest version
- **THEN** the system creates a new latest version with identical content and leaves the history intact

#### Scenario: Rollback to an unknown version
- **WHEN** a client requests rollback to a version identifier that does not exist for `{uuid}/report.docx`
- **THEN** the system responds with a not-found error and does not modify the file or its history

#### Scenario: Rollback of a non-existent file
- **WHEN** a client requests rollback of `{uuid}/missing.docx`
- **THEN** the system responds with a not-found error and makes no changes
