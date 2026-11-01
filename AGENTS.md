# AGENTS.md

Guidance for AI coding assistants (Claude Code, Codex, Cursor, Copilot, ...)
working with agent-fabrik, either to **use** it (write configs, call the API)
or to **change** it (Go code in this repo). The human-facing docs are
[README.md](README.md) and [examples/full.yaml](examples/full.yaml); this file
is the short version with the rules that are easy to get wrong.

## What it is

agent-fabrik is a single Go binary (`fabrik`) that hosts AI agents for many
users. Agents are defined in YAML: a **provider** (LLM), **connectors**
(tools) and **triggers** (HTTP, OpenAI API, MCP, webhook, cron, interval,
IMAP). In multi-tenant mode each tenant manages its own definitions over the
`/v1` API, stored in Postgres.

## Using agent-fabrik

### Minimal config

```yaml
providers:
  local: {type: ollama, model: qwen3}          # or anthropic, mistral, openai, ...
agents:
  helper:
    provider: local                            # optional if there is only one
    system: You are a concise, helpful assistant.
    tools: ["system.*"]
```

```bash
make build
./bin/fabrik -check                            # validate config, connect connectors, list tools
./bin/fabrik -run helper -input "What time is it?"
./bin/fabrik -chat helper                      # terminal chat
./bin/fabrik                                   # serve on :8080, web UI at /ui/
```

Config lookup without `-config`: `$AGENT_CONFIG`, then `$AGENT_CONFIG_YAML`
(inline), then `./agents.yaml`, then the built-in env-only config.

### Rules for writing configs

- **Every option is documented in [examples/full.yaml](examples/full.yaml).**
  Read it before inventing a key: unknown or misspelled keys are silently
  ignored, not rejected. Run `fabrik -check` after every config change.
- **Tool names.** In `tools:` use `connector.tool` or globs (`"mail.*"`,
  `"files.read"`). The model itself sees `connector__tool`. Built-in
  connectors: `system` (`now`) and `files` (tenant file storage).
- **Values.** `${VAR}`, `${VAR:-default}`, `${VAR:?error}`,
  `${file:/path}`; `$${` is a literal `${`. Expansion happens after YAML
  parsing, so secrets need no quoting.
- **Tenant documents** (sent to `PUT /v1/config`) may only use
  `${secret:NAME}` for credentials, never server env vars. Store secrets
  first with `PUT /v1/secrets/NAME` (`{"value": "..."}`).
- **Not allowed for tenants:** `shell` connectors, stdio MCP servers,
  `allow_private`. Shell only works single-tenant (`tenancy.enabled` false).
- **Mail agents:** `imap` trigger + `email` connector; the agent answers
  `NO_REPLY` to stay silent. Set `allow_from` and `allow_recipients`.
- **Trigger-only agents** should have `expose: false`.

### Calling agents

All `/v1/*` and `/mcp` routes need `Authorization: Bearer <token>` (admin
token, tenant key `ark_...`, or an external token with `external_auth`).

| Want to | Use |
|---|---|
| run once | `POST /v1/agents/{name}/run` `{"input": "...", "session_id": "..."}` |
| stream | same with `"stream": true` (SSE) |
| OpenAI client | base URL `http://host:8080/v1`, `model` = agent name |
| MCP client | `POST /mcp`, each agent is a tool |
| see available tools | `GET /v1/tools` |
| manage tenant config | `GET/PUT /v1/config`, `/v1/config/{providers,connectors,agents}/{name}` |

Without a token and without tenancy the API is open but bound to loopback
only; set `AGENT_RUNTIME_TOKEN` before serving on a network.

## Working on the code

### Commands

```bash
make build   # bin/fabrik (CGO_ENABLED=0, static)
make test    # unit + e2e tests with fake LLM, MCP, IMAP, SMTP servers
make race    # tests with the race detector
make lint    # gofmt + go vet; must be clean
```

Postgres store tests run only with `AR_TEST_DATABASE_URL` set (see README,
*Development*). Go 1.25.

### Layout

| Package | Contents |
|---|---|
| `main.go` | flags, config loading, wiring |
| `internal/config` | YAML schema, `${...}` expansion, validation, cron parsing |
| `internal/llm` | providers: native Anthropic + Ollama, OpenAI-compatible adapter with presets |
| `internal/agent` | agent loop, steps, sessions |
| `internal/tools` | connectors: MCP, HTTP, email, files, shell, system |
| `internal/mcp` | MCP client and server |
| `internal/mail` | IMAP, SMTP, MIME parsing |
| `internal/triggers` | heap scheduler, cron, IMAP polling |
| `internal/tenant` | per-tenant runtime cache, quotas, secrets, auth |
| `internal/store` | memory/file and Postgres stores |
| `internal/netguard` | SSRF guard for every tenant-controlled connection |
| `internal/server` | HTTP API, SSE, OpenAI, MCP, webhooks, tenant/admin API, web UI (`ui/`, plain HTML/CSS/JS, no build step) |

### Rules

- **No new dependencies.** Only `yaml.v3` and `lib/pq`. Protocols (MCP,
  SMTP, IMAP, LLM APIs) are implemented on the standard library.
- **Multi-tenancy first.** Anything a tenant can configure must be safe with
  thousands of strangers in one process: dial through `internal/netguard`,
  respect plan limits in `internal/tenant/quota.go`, never expose server env
  vars or secrets in API responses.
- **Tests.** New behavior needs a test; a bug fix needs a test that fails
  without it. Use the fakes in `internal/testutil` and the existing e2e
  tests in `internal/server` as patterns.
- **Docs.** A new or changed option updates `README.md` and
  `examples/full.yaml` (and this file if it changes a rule above).
- **Web UI** stays dependency-free and must keep working under the strict
  Content-Security-Policy (no inline scripts, no CDNs).
- **Commits.** One topic per PR, short imperative subject
  ("Webhooks: rate-limit before the lookup").
- Security issues go through [SECURITY.md](SECURITY.md), not public issues.
