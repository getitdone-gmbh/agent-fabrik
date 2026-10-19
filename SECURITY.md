# Security policy

agent-fabrik runs agents of many users in one process, so we take isolation bugs
seriously: tenant escapes, SSRF guard bypasses, secret leaks, auth bypasses
and anything that lets one tenant affect another.

## Reporting a vulnerability

Please **do not open a public issue.** Report it privately through
[GitHub's private vulnerability reporting](https://github.com/getitdone-gmbh/agent-fabrik/security/advisories/new).

Include what you found, how to reproduce it (config, requests) and the
version (`fabrik -version`). We confirm receipt within 3 working days, keep
you updated, and credit you in the advisory unless you prefer otherwise.

## Supported versions

Fixes go into the latest release. Until 1.0 there are no backports to older
versions.

## Scope

In scope: the agent-fabrik binary, its HTTP APIs, the web UI, the container image
and the example deployments in this repository.

Out of scope: what a model decides to do with the tools you give it (an
agent with a `shell` connector can run commands; that's the point), the
security of third-party MCP servers and LLM providers, and single-tenant
setups whose operator turned protections off on purpose (`allow_private`,
`inherit_env`, no `auth_token`).
