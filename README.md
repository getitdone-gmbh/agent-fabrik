<h1 align="center">agent-fabrik</h1>

<p align="center">
  <b>A factory floor for AI agents.</b><br>
  Thousands of agents of thousands of users at work around the clock, in one tiny process.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-blue.svg"></a>
  <img alt="Go 1.25" src="https://img.shields.io/badge/go-1.25-00ADD8.svg?logo=go&logoColor=white">
  <img alt="Dependencies: 2" src="https://img.shields.io/badge/dependencies-2-brightgreen.svg">
  <img alt="Binary: ~9 MB" src="https://img.shields.io/badge/binary-~9%20MB-informational.svg">
</p>

---

Most agent frameworks are libraries for one developer and one agent. Hosting
agents *for other people* is a different problem: every user wants their own
agents, keys, tools, schedules and limits, and you don't want a container per
agent sitting idle at 3 a.m.

**agent-fabrik** is that hosting layer. *Fabrik* is German for factory: one
hall, many robots, each doing its own job around the clock. You define agents
in YAML (or let your users do it over an API). Then you run them over HTTP,
the OpenAI API, MCP, webhooks, cron or email.

Agents are rows, not pods: an idle agent costs ~350 bytes in Postgres. 2,000
tenants with 10,000 agents and 6,000 triggers idle at **35 MB of RAM**.

- 🪶 **Small.** ~9 MB static binary, ~19 MB distroless image. Two dependencies
  (`yaml.v3`, `lib/pq`). MCP, SMTP, IMAP and every LLM API are implemented on
  the Go standard library.
- 🏢 **Multi-tenant by design.** Isolated tenants, plans and quotas, encrypted
  secrets, an SSRF guard, and sign-in through your existing identity provider.
- 🔌 **Speaks everything.** Claude, Mistral, Ollama, OpenAI, Gemini, Groq and
  more. Tools come from MCP servers, HTTP APIs and email. Agents are served as
  REST, OpenAI-compatible, MCP or SSE.
- ⏰ **Agents that act on their own.** Cron, intervals, webhooks and an IMAP
  inbox that answers in-thread.
- 🖥️ **Batteries included.** A web UI, Prometheus metrics, file storage per
  tenant, and horizontal scale-out on Postgres.

## Contents

[Quick start](#quick-start) ·
[How it works](#how-it-works) ·
[Multi-tenancy](#multi-tenancy) ·
[Web UI](#web-ui) ·
[API](#api) ·
[Files](#files) ·
[Shell](#shell) ·
[Email agents](#email-agents) ·
[Deploy](#deploy) ·
[Security](#security) ·
[Performance](#performance) ·
[Development](#development) ·
[License](#license)

## Quick start

No API key needed if you have [Ollama](https://ollama.com) running locally:

```bash
git clone https://github.com/getitdone-gmbh/agent-fabrik && cd agent-fabrik
make build

cat > agents.yaml <<'YAML'
providers:
  local: {type: ollama, model: gemma4}   # or qwen3, llama3.1, ... (needs tool support)
agents:
  helper:
    system: You are a concise, helpful assistant.
    tools: ["system.*"]
YAML

./bin/fabrik -run helper -input "What time is it in Tokyo right now?"
# → It is currently 1:32 PM on Friday, October 2, 2026, in Tokyo.
```

The agent called its `system__now` tool to answer. Now serve it:

```bash
./bin/fabrik -chat helper     # chat in the terminal
./bin/fabrik                  # serve on :8080, web UI at http://localhost:8080
./bin/fabrik -check           # validate the config and list every tool
```

Prefer a hosted model? Swap the provider:

```yaml
providers:
  claude: {type: anthropic, api_key: "${ANTHROPIC_API_KEY}"}
```

[`examples/full.yaml`](examples/full.yaml) documents every option.

## How it works

```
            triggers                         agent loop                      connectors (tools)
 HTTP API ─┐                         ┌──────────────────────┐          ┌─ MCP servers (HTTP + stdio)
 OpenAI API├─┐                       │ system prompt        │          ├─ any remote MCP (CRM, ...)
 MCP server├─┼──▶ agent ──▶ provider │ + history (sessions) │ ◀──────▶ ├─ HTTP APIs (typed endpoints,
 webhooks  ├─┘                       │ + tools, max steps   │          │   allowlisted requests)
 cron      │                         └──────────────────────┘          ├─ SMTP (send_email)
 IMAP inbox┘         Claude · Mistral · Ollama · OpenAI · Groq ·       ├─ IMAP (list/read/mark_read)
                     Gemini · OpenRouter · DeepSeek · vLLM · ...       └─ files (per tenant)
```

**Providers.** agent-fabrik has a native Anthropic Messages API client (Claude) and
a native Ollama `/api/chat` client. An OpenAI-compatible adapter has presets
for `openai`, `mistral`, `groq`, `openrouter`, `gemini`, `deepseek`,
`together`, `scaleway`, `ionos`, `xai`, `lmstudio`, `vllm` and Azure OpenAI.
Any other endpoint works as `openai-compatible`.

**Connectors** supply the tools:

| Type | Tools |
|---|---|
| `mcp` | any MCP server over streamable HTTP (JSON and SSE, sessions) or stdio, with include/exclude filters |
| `http` | declared endpoints become typed tools, plus a generic `request` tool limited to allowed hosts |
| `email` / `smtp` / `imap` | `send_email`, `list_emails`, `read_email`, `mark_read` |
| `shell` | `run` executes commands on your own machine (single tenant only, see [Shell](#shell)) |
| `files` (built in) | `files__list`, `files__read`, `files__write`, `files__delete` |
| `system` (built in) | `now` |

**Triggers.** You can start an agent in several ways:

- over HTTP, as JSON or an SSE stream;
- through an OpenAI-compatible `/v1/chat/completions`, where the agent name is the model;
- over MCP, where each agent is a tool at `/mcp`;
- by webhook, signed with GitHub HMAC or a shared secret;
- on a 5-field cron schedule with time zones, or at an interval;
- by polling an IMAP inbox, with threaded replies.

**Sessions.** Each session keeps its own conversation memory: in memory (TTL +
LRU), as files on a volume, or in Postgres. An email thread maps to a session
automatically.

### Config values

Config values support `${VAR}`, `${VAR:-default}`, `${VAR:?error message}`
and `${file:/run/secrets/key}`. They are expanded **after** YAML parsing, so a
secret may contain any character without breaking the document. `$${` gives a
literal `${`.

Without `-config`, agent-fabrik looks for a config in this order:

1. `$AGENT_CONFIG`
2. inline YAML in `$AGENT_CONFIG_YAML`
3. `./agents.yaml`
4. the built-in [`agents.yaml`](agents.yaml), which reads only env vars
   (`LLM_PROVIDER`, `LLM_API_KEY`, `LLM_MODEL`, `MCP_URL`,
   `TENANCY_ENABLED`, ...)

## Multi-tenancy

Turn on `tenancy` (see [`examples/multitenant.yaml`](examples/multitenant.yaml),
or [`examples/local.yaml`](examples/local.yaml) for a laptop without Postgres).
One process then serves many isolated users.

```bash
# As the operator: create a tenant and get its API key
curl -X POST $RT/v1/admin/tenants -H "Authorization: Bearer $ADMIN" -d '{"id":"alice"}'
# → {"key":"ark_...","tenant":{"id":"alice","plan":"free",...}}

# As alice: bring your own key, define an agent, run it
curl -X PUT $RT/v1/secrets/MISTRAL_KEY -H "Authorization: Bearer $KEY" -d '{"value":"..."}'
curl -X PUT $RT/v1/config -H "Authorization: Bearer $KEY" --data-binary @- <<'YAML'
providers:
  mistral: {type: mistral, api_key: "${secret:MISTRAL_KEY}"}
agents:
  support:
    provider: mistral
    system: You answer customer questions.
    triggers: [{type: cron, schedule: "0 8 * * 1-5", timezone: Europe/Berlin}]
YAML
curl $RT/v1/agents/support/run -H "Authorization: Bearer $KEY" -d '{"input":"hi"}'
```

**Tenants** are created by the admin or automatically for the users of your
identity provider or platform (`tenancy.external_auth`). Users sign in with
the token they already have (a JWT or an API key). agent-fabrik checks it against
a userinfo endpoint: the OIDC `/userinfo` of Keycloak, Zitadel, Auth0 or
Google, or any "who am I" API. Each user becomes a tenant (`ext-<id>`), and
the plan can come from a claim (`plan_field`, `plan_map`). With
`require_field` only users whose claim is true (e.g. `admin`) get in.

**Self-service.** Tenants manage their own providers, connectors, agents,
secrets and keys over the API. They use the same YAML schema as the config
file. Every change is validated in one transaction, so an agent can't
reference a missing provider and a secret still in use can't be deleted.

**Plans and limits.** You can limit each tenant by plan:

- the number of agents, connectors, providers, secrets and triggers;
- concurrent runs, runs per minute and monthly tokens;
- `max_steps` and the minimum cron and poll intervals;
- the allowed connector types;
- access to your **shared providers**, such as an operator-paid Mistral key
  or an in-cluster Ollama.

`server.max_concurrent_runs` caps the whole process. `GET /v1/me` shows each
tenant its limits and this month's usage.

**Cheap when idle.** A tenant's runtime (providers, MCP clients, agents) is
built on first use and shared by concurrent runs. It is rebuilt atomically
when the tenant's definitions change and unloaded after `runtime_idle_ttl`.
One heap-based scheduler with a bounded worker pool fires every tenant's
triggers. Start times are randomly offset, so thousands of agents don't fire
at once.

**Scale-out.** With the Postgres store, run as many replicas as you like:

- any replica serves runs;
- exactly one replica, chosen by a Postgres advisory lock, runs the scheduler
  and fails over automatically;
- runs of one session are serialized across replicas;
- per-process limits are split by `tenancy.replicas` (`AGENT_REPLICAS`);
- all locks are transaction-scoped, so the store also works behind PgBouncer
  in transaction mode.

The agents in the config file keep working as the read-only **system**
tenant, reached with the admin token.

<details>
<summary><b>Tenant and admin API</b></summary>

| Route | Purpose |
|---|---|
| `GET /v1/me` | tenant, effective limits, this month's usage |
| `GET /v1/config`, `PUT /v1/config[?prune=true]` | read / apply a whole YAML or JSON document (`prune` deletes what's missing) |
| `GET/PUT/DELETE /v1/config/{providers\|connectors\|agents}/{name}` | single definitions |
| `GET /v1/secrets`, `PUT/DELETE /v1/secrets/{name}` | write-only secret vault (`{"value": "..."}`) |
| `GET/POST /v1/keys`, `DELETE /v1/keys/{id}` | runtime API keys (`ark_...`, stored hashed) |
| `GET/PUT/DELETE /v1/files/{path}` | per-tenant file storage (see [Files](#files)) |
| `POST /v1/tenants/{tenant}/hooks/{agent}[/{name}]` | tenant webhooks |
| all routes under [API](#api) | scoped to the caller's tenant |

Admin routes use `server.auth_token`:

- `GET/POST /v1/admin/tenants`
- `GET/PATCH/DELETE /v1/admin/tenants/{id}` to change the plan, override
  limits or disable a tenant
- `POST /v1/admin/tenants/{id}/keys`
- `GET /v1/admin/stats`

The admin token with `X-Tenant: <id>` acts as that tenant (for support).

</details>

## Web UI

The binary ships a small web UI at `/ui/` (`/` redirects there). Sign in with
a tenant key (`ark_...`), your platform token (with `external_auth`) or the
admin token. In the UI you can:

- see your agents with their provider, tools and triggers, and chat with them
  (streamed, with tool calls shown);
- create, edit and delete agents in a form or as JSON;
- add providers (the API key is stored as an encrypted secret), connectors
  (from YAML templates), secrets and API keys;
- as admin, switch between the system agents and any tenant.

The page is static: plain HTML, CSS and JS embedded in the binary, with no
build step and no third-party code. It calls the same `/v1` API and is served
with a strict Content-Security-Policy. Without tenancy, the UI can list and
chat with the config-file agents but cannot change them.

Running agent-fabrik behind your own dashboard? Turn the UI off with `server.ui: false`.

## API

All `/v1/*` and `/mcp` routes require `Authorization: Bearer <token>` (or
`X-API-Key`). The token is one of these:

- the admin token;
- a tenant key;
- an external token, validated against your userinfo endpoint.

> [!WARNING]
> If no token is configured and tenancy is off, the API is open. agent-fabrik then
> listens on loopback only (`:8080` becomes `127.0.0.1:8080`) and refuses to
> start on any other address. Set `AGENT_RUNTIME_TOKEN` to serve the network.

| Route | Purpose |
|---|---|
| `GET /healthz`, `/readyz`, `/metrics` | health, Prometheus metrics |
| `GET /v1/agents`, `GET /v1/agents/{name}` | list / describe agents and their tools |
| `POST /v1/agents/{name}/run` | `{"input": "...", "session_id": "...", "stream": false}` returns `{output, session_id, steps, tool_calls, usage, ...}` |
| same with `"stream": true` or `Accept: text/event-stream` | SSE events `step`, `tool_call`, `tool_result`, `message`, `result`, `error` |
| `DELETE /v1/agents/{name}/sessions/{id}` | forget a session |
| `POST /v1/chat/completions`, `GET /v1/models` | OpenAI-compatible; `model` is the agent name |
| `POST /mcp` | MCP server: every exposed agent is a tool (`input`, `session_id`) |
| `POST /v1/hooks/{agent}[/{name}]` | webhook trigger (`X-Hub-Signature-256`, `X-Webhook-Secret`; `?secret=` only for operator hooks, never for tenants) |

```bash
curl -s localhost:8080/v1/agents/helper/run \
  -H "Authorization: Bearer $AGENT_RUNTIME_TOKEN" \
  -d '{"input":"What time is it in Tokyo?"}'
```

Any OpenAI client can talk to your agents. Point it at
`http://localhost:8080/v1` and use the agent name as the model.

Use your agents from Claude Code, Cursor and other MCP clients:

```bash
claude mcp add --transport http agents https://agents.example.com/mcp \
  --header "Authorization: Bearer $AGENT_RUNTIME_TOKEN"
```

CLI flags: `-config`, `-check`, `-chat <agent>`, `-run <agent> -input "..."`
(or stdin), `-version`.

## Files

Every tenant has its own file storage. It lives in the store, so with
Postgres all replicas share it and your database backups cover it.

```bash
curl -X PUT $RT/v1/files/customers/list.csv -H "Authorization: Bearer $KEY" --data-binary @list.csv
curl "$RT/v1/files?prefix=reports/" -H "Authorization: Bearer $KEY"
curl $RT/v1/files/reports/2026-09.md -H "Authorization: Bearer $KEY"
```

- Agents get access with `tools: ["files.*"]`. Use
  `tools: ["files.list", "files.read"]` for read-only access.
- `files__read` returns text in pieces for long files. `files__write` can
  create, overwrite or append.
- Plans cap storage with `max_files`, `max_file_mb` and `max_storage_mb`
  (defaults 500, 5 and 50; `-1` means unlimited). The quota is checked in the
  same transaction as the write, so parallel uploads can't overshoot it.
- Paths are relative. `..`, absolute paths and control characters are
  refused. Downloads are always sent as attachments with `nosniff`.

In multi-tenant mode there is deliberately no shell and no host file
system, because agents of many tenants share one process.

## Shell

On a machine of your own (a home server, a dev box) an agent may run
commands through a `shell` connector. You decide in the config what is
allowed. Full example: [`examples/homelab.yaml`](examples/homelab.yaml).

```yaml
connectors:
  sh:
    type: shell
    workdir: /home/ai/agent-work   # required; cwd may not leave it (symlinks included)
    timeout_seconds: 120           # default 30; the whole process group is killed
    deny: ["sudo *"]               # globs on the command line
  inspect:
    type: shell
    workdir: /home/ai/agent-work
    allow: [ls, cat, git, df]      # no shell, only these programs
agents:
  ops: {tools: ["sh.*"], tool_timeout_seconds: 150}
```

- **Two modes.** Without `allow` the command line runs as `bash -c`, so
  pipes, redirects and scripts work; `deny` then only catches the obvious
  (`*` also matches `/`, so `sudo /bin/rm` is caught, `s""udo x` is not). With `allow` there is no shell:
  the line is split into words, the program must match `allow` by name
  (`/bin/rm` does not match `rm`), and `;`, `|` and `$(...)` stay literal
  arguments. Programs that start others (`env`, `xargs`, `find -exec`,
  `git -c`) reopen the door, so keep them off the list when that matters.
- **Environment.** Commands get only `PATH`, `HOME`, `USER`, `LANG`, `TZ`
  and friends plus `env`; agent-fabrik's own API keys and tokens stay out unless
  `inherit_env: true`.
- **Limits.** Per command `timeout_seconds` and `max_output` bytes per
  stream; set the agent's `tool_timeout_seconds` above the connector timeout.
- **Not a sandbox.** Commands run with agent-fabrik's user rights. Run agent-fabrik as
  a dedicated unprivileged user, or in a container or VM, when the machine
  matters.
- **Never multi-tenant.** agent-fabrik refuses the connector when
  `tenancy.enabled` is true (a child can read the parent's environment via
  `/proc`) and refuses it in every tenant document regardless of plan.

## Email agents

Give an agent an inbox, and it answers its mail:

```yaml
connectors:
  mail:
    type: email
    smtp: {host: smtp.example.com, port: 587, username: bot@example.com, password: "${SMTP_PASSWORD}", from: "Support <bot@example.com>"}
    imap: {host: imap.example.com, port: 993, username: bot@example.com, password: "${IMAP_PASSWORD}"}
agents:
  support:
    system: You answer customer emails for ACME. Reply NO_REPLY to spam.
    tools: ["mail.*"]
    triggers: [{type: imap, connector: mail, every: 1m, allow_from: ["*@example.com"]}]
```

The `imap` trigger polls the mailbox (by default every minute). For each
unread message, agent-fabrik:

1. runs the agent with the sender, subject and plain-text body;
2. sends the answer as a threaded reply (`In-Reply-To`, `References`,
   `Auto-Submitted: auto-replied`);
3. marks the message as read.

Replies in the same thread from the same sender continue the same session.
Several rules prevent mail loops:

- bounces, auto-replies, list mail and `noreply` senders are skipped;
- the agent can answer `NO_REPLY` to stay silent;
- `allow_from` restricts senders, and `allow_recipients` on the connector
  restricts who `send_email` may write to. A `Reply-To` outside
  `allow_from` is ignored and the reply goes to `From`;
- a message that keeps failing is retried 3 times, then marked read.

`allow_from` is not authentication: anyone can forge a `From` header. Don't
give a mail-triggered agent tools you wouldn't hand to whoever can send it
mail, and set `allow_recipients` when it may send email.

## Deploy

**Binaries.** Every tag `v*` publishes static binaries for Linux and macOS
(`amd64`, `arm64`) with `SHA256SUMS` on the
[releases page](https://github.com/getitdone-gmbh/agent-fabrik/releases):

```bash
gh release download -R getitdone-gmbh/agent-fabrik -p '*linux_arm64*' -p SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf agent-fabrik_*_linux_arm64.tar.gz && ./agent-fabrik_*_linux_arm64/fabrik -version
```

Everything can be configured with env vars (see [`agents.yaml`](agents.yaml)).

| Target | File |
|---|---|
| Docker Compose (runtime + Postgres) | [`deploy/docker-compose.yaml`](deploy/docker-compose.yaml), `make up` |
| Kubernetes (2 replicas, hardened pod) | [`deploy/kubernetes/agent-fabrik.yaml`](deploy/kubernetes/agent-fabrik.yaml) |
| Cloud Native Buildpacks (`pack`, kpack, Heroku-style PaaS) | `go.mod` at the root + [`project.toml`](project.toml); listens on `$PORT` |
| ITDone Cloud | [`deploy/itdone/itdone.yaml`](deploy/itdone/itdone.yaml) |

**Single tenant:** leave `TENANCY_ENABLED` unset. No database is needed.

**Multi-tenant** needs these env vars:

| Variable | What it is |
|---|---|
| `TENANCY_ENABLED=true` | turns on tenancy |
| `DATABASE_URL` | the Postgres store |
| `RUNTIME_SECRET_KEY` | `openssl rand -base64 32`; keep it safe, it decrypts tenant secrets |
| `AGENT_RUNTIME_TOKEN` | the admin token |

To offer a model you pay for to all tenants, add `LLM_API_KEY` and
`TENANT_SHARED_LLM=true`. Add replicas for throughput, not per agent.

The `Dockerfile` builds a distroless, non-root image of about 19 MB.
`GOMEMLIMIT` (default `48MiB` in the image) is a soft heap cap. Raise it if
you run many concurrent agents with large contexts.

## Security

agent-fabrik runs other people's agents next to each other, so isolation is part of
the core.

- **Secrets** are encrypted with AES-256-GCM (`tenancy.secret_key`), bound to
  their tenant and name, and never returned by the API. Tenant documents can
  only reference `${secret:NAME}`, never server environment variables.
- **SSRF guard.** Every connection a tenant controls (LLM `base_url`, MCP,
  HTTP tools, IMAP, SMTP) is checked when it connects. Loopback, private,
  link-local and metadata, CGNAT and other internal ranges are blocked, which
  also covers DNS rebinding. Allow specific internal services with
  `allow_networks`.
- **No host access for tenants.** stdio MCP servers and `allow_private` are
  refused. Your own stdio MCP servers and shell commands get only `PATH`,
  `HOME` and locale plus their `env`, not agent-fabrik's keys and tokens
  (`inherit_env: true` to change that). This keeps an MCP server that logs
  or forwards its environment from leaking them; it is no sandbox against
  a malicious one running as the same user.
- **Namespacing.** Sessions, usage, files and webhooks are namespaced per
  tenant. Tenant webhooks always need their own secret.
- **Bounded tool calls.** Each tool call has a timeout, its output is
  truncated, and panics are recovered. Each run has a step limit
  (`max_steps`, default 12).
- **Credentials stay at home.** `http` and MCP connectors send their
  credentials only to their own scheme and host, and never follow a redirect
  away from it. Model-chosen URLs must match `allow_hosts`, and the same
  check applies to redirects.
- **Mail.** SMTP header values are sanitized, and STARTTLS is required unless
  you set `tls: none`.
- Hide trigger-only agents from the APIs with `expose: false`.

Found a vulnerability? Please report it privately, see
[SECURITY.md](SECURITY.md). Don't open a public issue.

## Performance

Measured on an Apple M-series machine with a fake LLM and real Postgres:

| Scenario | Result |
|---|---|
| Binary / container image | ~8.7 MB static binary, ~19 MB image, ~12 MiB RAM at start |
| Empty process | ~15 MB RSS, 0% CPU |
| **2,000 tenants, 10,000 agents, 6,000 triggers, idle** | **35 MB RSS**, ~0% CPU |
| 20,000 runs across all 2,000 tenants (all runtimes loaded) | 1,840 runs/s, 72 MB RSS (~18 KB per loaded tenant) |
| Single tenant, 10,000 runs (2 LLM calls + 1 tool each) | ~1,400 runs/s, 43 MB peak, ~0.4 ms CPU/run |
| Idle with 6,000 active triggers (~5 scheduled runs/s) | ~1% of one CPU core |
| Postgres per agent definition (row + index) | ~350 bytes (10,000 agents: 3.5 MB) |
| Postgres per stored session (short chat) | ~1.1 KB, expires after `session_ttl` |

In production, the LLM is the bottleneck, not agent-fabrik.

<details>
<summary><b>Claude specifics</b></summary>

- The default model is `claude-opus-5`.
- Assistant turns are replayed byte for byte (including thinking blocks), so
  multi-step tool loops stay valid.
- Server-side refusal fallbacks are on by default (`fallbacks: default`). Set
  `fallbacks: off` on the provider to disable them, for example behind a
  proxy that rejects the parameter.
- `effort` maps to `output_config.effort`.

</details>

## Development

```bash
make test    # unit + end-to-end tests (fake LLM, MCP, IMAP and SMTP servers)
make race    # with the race detector
make lint
```

The Postgres tests run when `AR_TEST_DATABASE_URL` is set:

```bash
docker run -d --rm --name ar-pg -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17-alpine
AR_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/postgres?sslmode=disable" make race
```

| Package | What's inside |
|---|---|
| `internal/llm` | providers |
| `internal/mcp` | MCP client + server |
| `internal/mail` | IMAP, SMTP, MIME |
| `internal/tools` | connectors |
| `internal/agent` | agent loop, sessions |
| `internal/triggers` | heap scheduler, cron, IMAP polling |
| `internal/tenant` | runtime cache, quotas, secrets, auth |
| `internal/store` | memory/file and Postgres stores |
| `internal/netguard` | SSRF guard |
| `internal/server` | HTTP, SSE, OpenAI, MCP, webhooks, tenant and admin API, web UI (`internal/server/ui`) |
| `internal/config` | config loading and validation |

Contributions are welcome, see [CONTRIBUTING.md](CONTRIBUTING.md). Please
open an issue first for larger changes.

## License

Copyright 2026 Get IT Done GmbH.

Licensed under the [Apache License, Version 2.0](LICENSE).
