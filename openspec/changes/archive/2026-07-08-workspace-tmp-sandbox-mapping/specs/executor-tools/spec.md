## ADDED Requirements

### Requirement: Sandbox /tmp mapped to workspace tmp directory
The `bash` and `python` sandboxes SHALL mount the user's `workspace/tmp/` directory at `/tmp` inside the sandbox so that temporary files persist after the sandbox exits and are reachable via `xizhi_*` workspace tools.

#### Scenario: Bash writes temporary file to /tmp
- **WHEN** the agent calls `bash` with `{"command": "echo hello > /tmp/hello.txt"}`
- **THEN** the file is written to `data/{user_uuid}/workspace/tmp/hello.txt`
- **AND** a subsequent `xizhi_read_file` with path `tmp/hello.txt` returns the content

#### Scenario: Python writes temporary file to /tmp
- **WHEN** the agent calls `python` with `{"code": "open('/tmp/out.txt','w').write('x')"}`
- **THEN** the file is written to `data/{user_uuid}/workspace/tmp/out.txt`
- **AND** a subsequent `xizhi_read_file` with path `tmp/out.txt` returns the content

#### Scenario: workspace/tmp created on demand
- **WHEN** the agent calls `bash` and `workspace/tmp/` does not yet exist
- **THEN** the system creates `workspace/tmp/` before mounting the sandbox
- **AND** the command succeeds

## MODIFIED Requirements

None.

## REMOVED Requirements

None.
