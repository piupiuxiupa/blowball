# workspace-file-rename Specification

## Purpose

定义工作空间内文件或目录的重命名能力，支持在相对 workspace 的任意路径间移动，并禁止覆盖已存在的目标。

## Requirements

### Requirement: Rename workspace file or directory
The system SHALL provide an authenticated endpoint to rename or move a file or directory within the user's workspace. Both the source and destination paths SHALL be validated by `xizhi.ValidatePath`.

Destination resolution:
- If `new_path` resolves to an **existing directory**, the source SHALL be moved *inside* it as `new_path/<basename(source)>` (the "move-into-folder" gesture); the final destination is then `new_path/<basename(source)>`.
- Otherwise the final destination is `new_path` itself; the destination's parent directory SHALL be created with `MkdirAll` if missing (enabling moves into new subdirectories).

If the final destination already exists:
- When `overwrite` is absent or `false`, the operation SHALL fail with HTTP 409 `ALREADY_EXISTS` without making any changes (preserves the prior behavior for an existing file destination).
- When `overwrite` is `true` and the final destination is an existing **file**, the system SHALL atomically replace it via `os.Rename`.
- When `overwrite` is `true` and the final destination is an existing **directory**, the system SHALL reject with HTTP 409 `DEST_NOT_EMPTY` (directory merge is not supported).

The workspace root SHALL not be renamable (HTTP 400).

#### Scenario: Rename file successfully
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/old.md` with body `{"new_path": "new.md"}` and `new.md` does not exist
- **THEN** the system renames the file and returns HTTP 200 with body `{"old_path": "old.md", "new_path": "new.md"}`

#### Scenario: Rename directory successfully
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/old-dir` with body `{"new_path": "new-dir"}` and `new-dir` does not exist
- **THEN** the system renames the directory and returns HTTP 200 with body `{"old_path": "old-dir", "new_path": "new-dir"}`

#### Scenario: Move file to a different directory
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/a.md` with body `{"new_path": "subdir/b.md"}` and `subdir/b.md` does not exist
- **THEN** the system moves the file and returns HTTP 200 with body `{"old_path": "a.md", "new_path": "subdir/b.md"}`

#### Scenario: Move file into an existing folder
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/a.md` with body `{"new_path": "subdir"}` and `subdir` is an existing directory
- **THEN** the system moves the file inside it as `subdir/a.md` and returns HTTP 200 with body `{"old_path": "a.md", "new_path": "subdir/a.md"}`

#### Scenario: Move directory into an existing folder
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/old-dir` with body `{"new_path": "parent"}` and `parent` is an existing directory
- **THEN** the system moves the directory inside it as `parent/old-dir` and returns HTTP 200 with body `{"old_path": "old-dir", "new_path": "parent/old-dir"}`

#### Scenario: Destination file already exists without overwrite
- **WHEN** an authenticated user sends a rename where the final destination resolves to an existing file and `overwrite` is absent or false
- **THEN** the system returns HTTP 409 with body `{"error": {"code": "ALREADY_EXISTS", "message": "destination already exists"}}` and changes nothing

#### Scenario: Overwrite an existing file destination
- **WHEN** an authenticated user sends a rename where the final destination resolves to an existing file and `overwrite` is true
- **THEN** the system atomically replaces the destination file and returns HTTP 200 with body `{"old_path": "<src>", "new_path": "<dst>"}`

#### Scenario: Overwriting a directory destination is rejected
- **WHEN** the final destination resolves to an existing directory and `overwrite` is true
- **THEN** the system returns HTTP 409 with body `{"error": {"code": "DEST_NOT_EMPTY", "message": "destination directory is not empty; merge not supported"}}` and changes nothing

#### Scenario: Source does not exist
- **WHEN** an authenticated user sends a rename request for a source path that does not exist
- **THEN** the system returns HTTP 404 with body `{"error": {"code": "NOT_FOUND", "message": "source not found"}}`

#### Scenario: Path outside workspace
- **WHEN** an authenticated user sends a rename request where either the source or `new_path` resolves outside the workspace
- **THEN** the system returns HTTP 403 with body `{"error": {"code": "FORBIDDEN", "message": "path outside workspace"}}`

#### Scenario: Missing authentication
- **WHEN** a request is sent without a valid JWT
- **THEN** the system returns HTTP 401
