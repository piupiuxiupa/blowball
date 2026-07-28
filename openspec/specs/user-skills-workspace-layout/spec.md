# user-skills-workspace-layout Specification

## Purpose

Define the canonical location of per-user skills under a reserved, application-owned `.blowball` namespace inside each user's workspace, and establish `.blowball` as a reserved workspace-internal namespace that user-facing file tooling must not surface as ordinary user content.

## Requirements

### Requirement: Per-user skills live under the workspace in a reserved hidden directory
The per-user skills directory SHALL be located at `data/{userID}/workspace/.blowball/skills/`. The `.blowball` directory is a reserved, application-owned namespace beneath the user's workspace; `skills/` is the sub-location for per-user skills. No top-level `data/{userID}/skills/` directory SHALL be created or read for per-user skills.

#### Scenario: Resolve per-user skills directory
- **WHEN** any component resolves the per-user skills directory for `userID`
- **THEN** the resolved path is `data/{userID}/workspace/.blowball/skills/`

#### Scenario: User directories ensure the skills sub-location
- **WHEN** `EnsureUserDirs` creates the canonical per-user directories for `userID`
- **THEN** `data/{userID}/workspace/.blowball/skills/` exists
- **AND** no top-level `data/{userID}/skills/` directory is created

### Requirement: Reserved workspace-internal namespace
`.blowball` SHALL be treated as a reserved namespace under each user's workspace. The application MAY use other `.blowball/*` sub-locations in the future; only `.blowball/skills` is defined by this capability.

#### Scenario: Reserved namespace is hidden from workspace listings
- **WHEN** workspace file listings are produced (REST API or `xizhi_*` discovery tools) without explicit hidden inclusion
- **THEN** the `.blowball` directory and its contents do not appear, consistent with the dotfile-hiding rule

#### Scenario: Reserved namespace remains hidden even with hidden inclusion
- **WHEN** workspace file listings are produced with hidden entries included
- **THEN** operator/user tooling may still choose to surface dotfiles, but `.blowball` is identified as a reserved application namespace, not user content
