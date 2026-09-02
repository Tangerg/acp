# Report a security vulnerability

Report vulnerabilities privately so a fix can be prepared before public details
make users easier to attack. Do not include exploit details in a public issue.

## Supported versions

This module is pre-1.0. Security fixes target the latest release only:

| Version | Supported |
| --- | --- |
| Latest `0.x` release | Yes |
| Earlier releases | No |

Upgrade before reporting a problem that may already be fixed.

## Send a private report

Use [GitHub private vulnerability
reporting](https://github.com/Tangerg/acp/security/advisories/new).
Include enough information to reproduce and bound the issue:

- Affected module version and the negotiated protocol version
- Go version, operating system, and transport such as subprocess stdio
- Minimal program, test, or JSON-RPC message sequence that triggers the behaviour
- Security impact and the boundary crossed
- Any known workaround

Maintainers triage the report privately. After confirming the impact and fix,
they coordinate publication, credit, and release timing in the advisory.

## Security scope

An agent reads and writes a user's workspace on a model's instructions. Security
boundaries therefore include:

- File paths received from a peer
- Permission requests and their answers
- Terminal and subprocess ownership
- Decoder resource limits
- Credentials in authentication messages

A flaw in the protocol itself belongs upstream with the [Agent Client Protocol
specification](https://github.com/agentclientprotocol/agent-client-protocol). A
normal library bug belongs in the public
[bug report form](https://github.com/Tangerg/acp/issues/new?template=bug.yml).
