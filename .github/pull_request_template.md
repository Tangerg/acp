# Pull request

## Outcome

Describe the caller-visible result and the problem it resolves.

## Design

State the owning type, value lifetime, dependency direction, and any rejected
alternative. Include benchmark evidence for performance claims. If the change
touches the wire, name the schema version it follows.

## Verification

- [ ] Added or updated an external-package test for public behavior
- [ ] Added a changelog entry for caller-visible or breaking changes
- [ ] Kept the wire grammar in step with the published ACP schema
- [ ] Ran `gofumpt`, `shfmt`, and the tests
- [ ] Ran race, lint, vulnerability, and Markdown gates when relevant
