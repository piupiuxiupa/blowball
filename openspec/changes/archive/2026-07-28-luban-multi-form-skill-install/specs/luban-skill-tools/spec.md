## ADDED Requirements

### Requirement: Sub-skill selection from skill collections
`luban_install_skill` SHALL accept an optional `skill` parameter that, for git-repo sources, selects a single sub-skill from the cloned collection and installs only that sub-skill. Selection matches the `skill` value against a discovered sub-skill's frontmatter `name`; if no unique name match is found, it falls back to matching `skill` as a repo-relative subpath whose target directory contains a `SKILL.md`. Only the selected sub-skill is installed; the remainder of the clone is discarded. The existing `name` parameter overrides the installed directory name; otherwise the installed name is the selected sub-skill's frontmatter name (or the `skill` value).

#### Scenario: Select a sub-skill by frontmatter name
- **WHEN** `luban_install_skill` is called with a collection repo URL and `skill: "gildata-finance-data"`
- **AND** the repo contains a sub-skill whose frontmatter `name` is `gildata-finance-data`
- **THEN** only that sub-skill is installed into the user skills directory
- **AND** no other sub-skills from the repo are installed

#### Scenario: Select a sub-skill by repo-relative subpath
- **WHEN** `luban_install_skill` is called with `skill: "skills/gildata-finance-data"` and no sub-skill frontmatter name matches
- **AND** the repo-relative dir `skills/gildata-finance-data` contains a `SKILL.md`
- **THEN** only that sub-skill is installed

#### Scenario: Unknown sub-skill lists the available ones
- **WHEN** `luban_install_skill` is called with a `skill` value that matches no discovered sub-skill by name or subpath
- **THEN** the tool returns an error listing the discovered sub-skill names so the caller can retry with a valid value
- **AND** nothing is written to the user skills directory

#### Scenario: name overrides the installed directory for a selected sub-skill
- **WHEN** `luban_install_skill` is called with both `skill: "X"` and `name: "my-name"`
- **THEN** the selected sub-skill X is installed under the directory name `my-name`

#### Scenario: skill parameter ignored for single-file sources
- **WHEN** `luban_install_skill` is called with a single SKILL.md URL and a `skill` value
- **THEN** the single SKILL.md is installed normally
- **AND** the result notes that `skill` does not apply to single-file sources

### Requirement: Install documentation URL handling
When `luban_install_skill` fetches a single-file (`.md`) URL whose body is not a valid SKILL.md (no YAML frontmatter, or frontmatter missing `name`/`description`), the tool SHALL NOT treat this as an install failure. It SHALL return a structured, non-error result identifying the content as an install document and including the fetched body, so the calling agent can read it, determine the real skill source, and call `luban_install_skill` again with that source. A body that does carry valid skill frontmatter is still installed as a skill as before.

#### Scenario: Non-skill markdown returned as install doc
- **WHEN** `luban_install_skill` is called with `https://skillhub.cn/install/skillhub.md`
- **AND** the fetched body is prose describing how to install `gildata-finance-data`, with no valid skill frontmatter
- **THEN** the tool returns a result with `installed: false` and `kind: "install-doc"`
- **AND** the result includes the fetched `content` and a hint to read it and re-install from the real source
- **AND** nothing is written to the user skills directory

#### Scenario: Valid SKILL.md still installs
- **WHEN** `luban_install_skill` is called with a `.md` URL whose body has valid skill frontmatter
- **THEN** the tool installs it as a single skill as before (existing behavior unchanged)

#### Scenario: Non-200 or non-text response still errors
- **WHEN** the single-file fetch returns a non-200 status or a non-text body
- **THEN** the tool returns an error rather than an install-doc result
