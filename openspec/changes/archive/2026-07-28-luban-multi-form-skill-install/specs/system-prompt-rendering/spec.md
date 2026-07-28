## ADDED Requirements

### Requirement: Multi-form skill install guidance
When the system prompt renders a Skills section, it SHALL include guidance describing the install shapes supported by `luban_install_skill` and how to handle install-documentation URLs. Specifically, the guidance SHALL convey: (a) `luban_install_skill` can install a whole git repo, a single sub-skill selected via the `skill` parameter, or a single `SKILL.md`; (b) if a `.md` URL is not itself a skill, the tool returns the document content rather than installing, and the agent SHOULD read that content, determine the real source URL, and call `luban_install_skill` again with it; (c) when a user asks to install from an instruction page, the agent SHOULD follow that page to the real artifact instead of assuming the page itself is the skill.

#### Scenario: Install guidance describes the supported shapes
- **WHEN** the system prompt renders a Skills section for an agent with `luban_install_skill` available
- **THEN** the section describes whole-repo, sub-skill (`skill`), and single-SKILL.md installs

#### Scenario: Install guidance describes the install-doc flow
- **WHEN** the system prompt renders a Skills section
- **THEN** the section tells the agent that a non-skill `.md` URL is returned as an install document to read, and that the agent should follow it to the real source and re-install

#### Scenario: Agent follows an instruction page to the real source
- **WHEN** the user asks to install a skill from an instruction-page URL
- **THEN** the rendered guidance directs the agent to read the returned install-doc content and install the real source it points at, rather than treating the page as the skill
