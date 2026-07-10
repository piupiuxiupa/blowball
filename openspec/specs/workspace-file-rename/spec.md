# workspace-file-rename Specification

## Purpose

定义工作空间内文件或目录的重命名能力，支持在相对 workspace 的任意路径间移动，并禁止覆盖已存在的目标。

## Requirements

### Requirement: Rename workspace file or directory
The system SHALL provide an authenticated endpoint to rename a file or directory within the user's workspace. Both the source and destination paths SHALL be validated by `xizhi.ValidatePath`. If the destination path already exists (as a file or directory), the operation SHALL fail with HTTP 409 without making any changes.

#### Scenario: Rename file successfully
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/old.md` with body `{"new_path": "new.md"}` and `new.md` does not exist
- **THEN** the system renames the file and returns HTTP 200 with body `{"old_path": "old.md", "new_path": "new.md"}`

#### Scenario: Rename directory successfully
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/old-dir` with body `{"new_path": "new-dir"}` and `new-dir` does not exist
- **THEN** the system renames the directory and returns HTTP 200 with body `{"old_path": "old-dir", "new_path": "new-dir"}`

#### Scenario: Move file to a different directory
- **WHEN** an authenticated user sends `PUT /api/v1/workspace/files/a.md` with body `{"new_path": "subdir/b.md"}` and `subdir/b.md` does not exist
- **THEN** the system moves the file and returns HTTP 200 with body `{"old_path": "a.md", "new_path": "subdir/b.md"}`

#### Scenario: Destination already exists
- **WHEN** an authenticated user sends a rename request where `new_path` resolves to an existing file or directory
- **THEN** the system returns HTTP 409 with body `{"error": {"code": "ALREADY_EXISTS", "message": "destination already exists"}}`

#### Scenario: Source does not exist
- **WHEN** an authenticated user sends a rename request for a source path that does not exist
- **THEN** the system returns HTTP 404 with body `{"error": {"code": "NOT_FOUND", "message": "source not found"}}`

#### Scenario: Path outside workspace
- **WHEN** an authenticated user sends a rename request where either `old_path` or `new_path` resolves outside the workspace
- **THEN** the system returns HTTP 403 with body `{"error": {"code": "FORBIDDEN", "message": "path outside workspace"}}`

#### Scenario: Missing authentication
- **WHEN** a request is sent without a valid JWT
- **THEN** the system returns HTTP 401
