## ADDED Requirements

### Requirement: Bucket versioning is ensured at startup
The system SHALL, on startup, verify that the configured MinIO bucket exists and has Object Versioning enabled. If the bucket does not exist, the system SHALL create it and enable versioning. The system SHALL fail fast if versioning cannot be enabled.

#### Scenario: Bucket does not exist
- **WHEN** the service starts and the configured bucket does not exist in MinIO
- **THEN** the service creates the bucket and enables Object Versioning on it before serving requests

#### Scenario: Bucket exists but versioning is off
- **WHEN** the service starts and the bucket exists with versioning suspended or disabled
- **THEN** the service enables Object Versioning on the bucket before serving requests

#### Scenario: Versioning cannot be enabled
- **WHEN** the service cannot enable versioning on the bucket (e.g., insufficient permissions)
- **THEN** the service fails fast and does not start serving requests

### Requirement: Upload a file as a new version
The system SHALL accept a file upload to `{uuid}/{filename}` and store it so that MinIO generates a new object version, preserving all prior versions. The `{uuid}` directory SHALL be created implicitly by the upload — no separate directory-creation step is required. The system SHALL return the new version identifier and timestamp.

#### Scenario: First upload of a file in a UUID directory
- **WHEN** a client uploads `report.docx` under UUID `abc` and no object `{uuid}/report.docx` exists yet
- **THEN** the system stores the file at key `abc/report.docx`, returns the new `versionId`, and the file becomes the latest version

#### Scenario: Subsequent upload creates a new version
- **WHEN** a client uploads `report.docx` under UUID `abc` again
- **THEN** the system stores a new version of `abc/report.docx`, the previous content is retained as a prior version, and the new upload becomes the latest version

#### Scenario: Filename collision across UUIDs is isolated
- **WHEN** two different UUIDs each upload a file named `report.docx`
- **THEN** the two files have independent version histories and do not interfere with each other

### Requirement: List version history of a file
The system SHALL return the version history of `{uuid}/{filename}`, ordered newest-first, where each entry includes the version identifier, last-modified timestamp, size in bytes, and whether it is the latest version.

#### Scenario: File with multiple versions
- **WHEN** a client requests the history of `{uuid}/report.docx` which has been uploaded three times
- **THEN** the system returns three version entries ordered newest-first, with exactly one entry marked as latest

#### Scenario: File does not exist
- **WHEN** a client requests the history of `{uuid}/missing.docx` which has never been uploaded
- **THEN** the system responds with a not-found error

### Requirement: Download the latest version
The system SHALL return the content of the latest version of `{uuid}/{filename}`.

#### Scenario: Download latest
- **WHEN** a client requests the latest version of `{uuid}/report.docx`
- **THEN** the system streams the content of the current latest version with an appropriate content type

#### Scenario: Latest version of a non-existent file
- **WHEN** a client requests the latest version of `{uuid}/missing.docx`
- **THEN** the system responds with a not-found error

### Requirement: Download a specific historical version
The system SHALL return the content of a specific version of `{uuid}/{filename}` identified by its version identifier.

#### Scenario: Download a historical version
- **WHEN** a client requests version `v2` of `{uuid}/report.docx`
- **THEN** the system streams the exact content of that historical version, even if newer versions exist

#### Scenario: Unknown version identifier
- **WHEN** a client requests a version identifier that does not exist for the file
- **THEN** the system responds with a not-found error

### Requirement: List files in a UUID directory
The system SHALL return the list of distinct files (current/latest objects) stored under a given `{uuid}/` prefix.

#### Scenario: Directory with several files
- **WHEN** a client lists the contents of UUID `abc` which contains `report.docx` and `budget.xlsx`
- **THEN** the system returns both filenames, each referencing its latest version

#### Scenario: Empty or non-existent directory
- **WHEN** a client lists a UUID that has no uploaded files
- **THEN** the system returns an empty list

### Requirement: List UUID directories
The system SHALL return the list of UUID directories (top-level prefixes) that contain at least one file.

#### Scenario: Multiple external systems have uploaded
- **WHEN** a client requests the list of UUID directories and UUIDs `abc` and `xyz` contain files
- **THEN** the system returns `abc` and `xyz` as top-level directories

### Requirement: Delete a file and all its versions
The system SHALL delete `{uuid}/{filename}` including every historical version of that object.

#### Scenario: Delete a versioned file
- **WHEN** a client deletes `{uuid}/report.docx` which has three versions
- **THEN** the system removes the object and all of its versions, and subsequent history/download requests for that file respond with not-found
