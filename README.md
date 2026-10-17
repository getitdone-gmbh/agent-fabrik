# agentd: a tiny multi-tenant agent runtime

A tiny, single-binary, **multi-tenant** runtime for LLM agents. One process
runs the agents of many users. Agents are rows in Postgres, not pods. Written in Go with two
dependencies (`gopkg.in/yaml.v3`, `github.com/lib/pq`). Every protocol
(MCP, HTTP, SMTP, IMAP) and every LLM API is implemented on the standard library.

| Measured (Apple M-series, fake LLM, real Postgres) | |
|---|---|
| Binary / container image | ~8.7 MB static binary, ~19 MB image; container uses ~12 MiB RAM at start |
| Empty process | ~15 MB RSS, 0% CPU |
| **2,000 tenants, 10,000 agents, 6,000 triggers, idle** | **35 MB RSS**, ~0% CPU |
| 20,000 runs across all 2,000 tenants (all runtimes loaded) | 1,840 runs/s, 72 MB RSS (~18 KB per loaded tenant) |
| Single tenant, 10,000 runs (2 LLM calls + 1 tool each) | ~1,400 runs/s, 43 MB peak, ~0.4 ms CPU/run |
| Idle with 6,000 active triggers (~5 scheduled runs/s) | ~1% of one CPU core |
| Postgres per agent definition (row + index) | ~350 bytes (10,000 agents: 3.5 MB) |
| Postgres per stored session (short chat) | ~1.1 KB, expires after `session_ttl` |

## What it does

```
            triggers                         agent loop                      connectors (tools)
 HTTP API ─┐                         ┌──────────────────────┐          ┌─ MCP servers (HTTP + stdio)
 OpenAI API├─┐                       │ system prompt        │          ├─ any remote MCP (CRM, ...)
 MCP server├─┼──▶ agent ──▶ provider │ + history (sessions) │ ◀──────▶ ├─ HTTP APIs (typed endpoints,
 webhooks  ├─┘                       │ + tools, max steps   │          │   allowlisted requests)
 cron      │                         └──────────────────────┘          ├─ SMTP (send_email)
 IMAP inbox┘         Claude · Mistral · Ollama · OpenAI · Groq ·       └─ IMAP (list/read/mark_read)
                     Gemini · OpenRouter · DeepSeek · vLLM · ...
```

- **LLM providers:** native Anthropic Messages API (Claude), native Ollama
  `/api/chat`, and an OpenAI-compatible adapter with presets for `openai`,
  `mistral`, `groq`, `openrouter`, `gemini`, `deepseek`, `together`,
  `scaleway`, `ionos`, `xai`, `lmstudio`, `vllm` and Azure OpenAI. Any
  other endpoint works as `openai-compatible`.
- **Connectors** supply tools:
  - `mcp`: any MCP server over streamable HTTP (JSON and SSE responses,
    sessions) or stdio subprocesses, with include/exclude filters.
  - `http`: declared endpoints become typed tools, plus a generic `request`
    tool limited to allowed hosts (private IPs blocked by default).
  - `email` / `smtp` / `imap`: `send_email`, `list_emails`, `read_email`,
    `mark_read`.
  - `shell`: `run` executes commands in a working directory on the host,
    either through bash or, with `allow`, only listed programs without a
    shell. Single-tenant only, see [Shell](#shell).
  - `system` (built in): `now`.
- **Triggers:** HTTP (JSON or SSE streaming), an OpenAI-compatible
  `/v1/chat/completions` (the agent name is the model), MCP (each agent is
  a tool at `/mcp`), webhooks (GitHub HMAC or a shared secret), 5-field cron
  schedules with time zones, intervals, and IMAP inbox polling with automatic
  threaded replies.
- **Sessions:** conversation memory per session, in memory (TTL + LRU) or
  as files on a volume. Email threads map to sessions automatically.

## Multi-tenancy: many users, one runtime

Enable `tenancy` (see [`examples/multitenant.yaml`](examples/multitenant.yaml)),
or set `TENANCY_ENABLED=true` with the embedded config. Then:

- **Tenants** are isolated accounts. They are created by the admin
  (`POST /v1/admin/tenants`) or automatically for **users of your identity
  provider or platform** (`tenancy.external_auth`). They authenticate with
  their existing token (JWT or API key), which the runtime validates via a
  userinfo endpoint: OIDC `/userinfo` of Keycloak, Zitadel, Auth0 or Google,
  or any "who am I" API. Each user gets a tenant (`ext-<id>`); the plan can
  come from a claim (`plan_field`, `plan_map`).
- **Self-service:** tenants manage their own providers, connectors, agents
  and secrets over the API, using the same YAML schema as the config file:

  ```bash
  curl -X PUT $RT/v1/secrets/MISTRAL_KEY -H "Authorization: Bearer $TOKEN" -d '{"value":"..."}'
  curl -X PUT $RT/v1/config -H "Authorization: Bearer $TOKEN" --data-binary @- <<'YAML'
  providers:
    mistral: {type: mistral, api_key: "${secret:MISTRAL_KEY}"}
  agents:
    support:
      provider: mistral
      system: You answer customer questions.
      triggers: [{type: cron, schedule: "0 8 * * 1-5", timezone: Europe/Berlin}]
  YAML
  curl $RT/v1/agents/support/run -H "Authorization: Bearer $TOKEN" -d '{"input":"hi"}'
  ```
- **Low cost per agent:** a tenant's runtime (providers, MCP clients, agents)
  is built on first use, shared by concurrent runs, rebuilt atomically when
  its definitions change, and unloaded after `runtime_idle_ttl`. One
  heap-based scheduler with a bounded worker pool fires every tenant's cron,
  interval and IMAP triggers, with random start offsets so thousands of
  agents don't fire at once. An idle agent costs a database row plus a heap
  entry per trigger.
- **Plans and limits** per tenant: agents, connectors, providers, secrets and
  triggers; concurrent runs; runs per minute; monthly tokens; `max_steps`;
  minimum cron/poll intervals; allowed connector types; use of the
  operator's **shared providers** (e.g. an operator-paid Mistral key or an
  in-cluster Ollama). A global `server.max_concurrent_runs` caps the whole
  process. Usage is metered per month (`GET /v1/me`).
- **Isolation and security:**
  - Secrets are encrypted with AES-256-GCM (`tenancy.secret_key`) and bound
    to tenant and name. They are never returned by the API.
  - Tenant documents can only reference `${secret:NAME}`, never server
    environment variables.
  - stdio MCP servers and `allow_private` are refused.
  - Every tenant-controlled connection (LLM `base_url`, MCP, HTTP tools,
    IMAP, SMTP) goes through an SSRF guard that blocks loopback, private,
    link-local/metadata, CGNAT and other internal ranges at connect time, so
    DNS rebinding is covered. Specific internal services can be allowed with
    `allow_networks`.
  - Sessions, usage and webhooks are namespaced per tenant. Tenant webhooks
    always need their own secret.
  - Every change is validated transactionally: an agent can't reference a
    missing provider, and a secret that is still in use can't be deleted.
- **Scale-out:** with the Postgres store, run several replicas behind the
  service. Runs are served by any replica. Exactly one replica (a Postgres
  advisory lock) runs the scheduler, picks up changes from all replicas, and
  fails over automatically. Runs of one session are serialized across
  replicas with a Postgres lock, and per-process limits (runs per minute,
  concurrent runs) are split by `tenancy.replicas` (`AGENT_REPLICAS`). All
  locks are transaction-scoped, so the store also works behind PgBouncer in
  transaction mode.
- The agents in the config file keep working as the read-only **system**
  tenant, reached with the admin token.

### Tenant API

| Route | Purpose |
|---|---|
| `GET /v1/me` | tenant, effective limits, this month's usage |
| `GET /v1/config`, `PUT /v1/config[?prune=true]` | read / apply a whole YAML or JSON document (`prune` deletes what's missing) |
| `GET/PUT/DELETE /v1/config/{providers\|connectors\|agents}/{name}` | single definitions |
| `GET /v1/secrets`, `PUT/DELETE /v1/secrets/{name}` | write-only secret vault (`{"value": "..."}`) |
| `GET/POST /v1/keys`, `DELETE /v1/keys/{id}` | runtime API keys (`ark_...`, stored hashed); `POST {"name","scope":"run","agents":[...]}` issues a key that can only run (those) agents, for app backends |
| `POST /v1/tenants/{tenant}/hooks/{agent}[/{name}]` | tenant webhooks |
| all routes under **API** below | scoped to the caller's tenant |

Admin (with `server.auth_token`): `GET/POST /v1/admin/tenants`,
`GET/PATCH/DELETE /v1/admin/tenants/{id}` (plan, limits override, disable),
`POST /v1/admin/tenants/{id}/keys`, `GET /v1/admin/stats`. The admin token
with `X-Tenant: <id>` acts as that tenant (support).

## Install (Linux)

Every tag `v*` publishes static binaries for `linux/amd64` and `linux/arm64`
with `SHA256SUMS` (tags with a `-` are pre-releases). The repository is
private, so download with `gh`:

```bash
gh release download v0.1.0-rc.1 -R getitdone-gmbh/agentd -p '*linux_amd64*' -p SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf agentd_*_linux_amd64.tar.gz && cd agentd_*_linux_amd64
./agentd -version
```

The archive contains `README.md`, `agents.yaml` and `examples/`. Build a
release locally with `make release`.

## Quick start

```bash
make build
export ANTHROPIC_API_KEY=...        # or MISTRAL_API_KEY, or run Ollama locally
./bin/agentd -config examples/full.yaml -check      # validate + list tools
./bin/agentd -config examples/full.yaml -chat chat  # terminal chat
./bin/agentd -config examples/full.yaml             # serve on :8080
```

Minimal config:

```yaml
providers:
  claude: {type: anthropic, api_key: "${ANTHROPIC_API_KEY}"}
agents:
  helper:
    system: You are helpful.
    tools: ["system.*"]
```

Without `-config`, the runtime uses `$AGENT_CONFIG`, then inline YAML in
`$AGENT_CONFIG_YAML`, then `./agents.yaml`, and finally the built-in
[`agents.yaml`](agents.yaml), which is driven only by env vars
(`LLM_PROVIDER`, `LLM_API_KEY`, `LLM_MODEL`, `MCP_URL`, `TENANCY_ENABLED`, ...).

[`examples/full.yaml`](examples/full.yaml) documents every option.

### Config values

`${VAR}`, `${VAR:-default}`, `${VAR:?error message}` and
`${file:/run/secrets/key}` are expanded **after** YAML parsing, so secrets
may contain any character without breaking the document. `$${` gives a
literal `${`.

## API

All `/v1/*` and `/mcp` routes require `Authorization: Bearer <token>`
(or `X-API-Key`). The token is the admin token (`server.auth_token`), a
tenant key, or an external (userinfo-validated) token. Without any token and without tenancy, the
runtime logs a warning and the API is open. Only use that locally.

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
curl -s localhost:8080/v1/agents/chat/run \
  -H "Authorization: Bearer $AGENT_RUNTIME_TOKEN" \
  -d '{"input":"What time is it in Tokyo?"}'
```

Use it from Claude Code, Cursor, etc. as an MCP server:

```bash
claude mcp add --transport http agents https://agents.example.com/mcp \
  --header "Authorization: Bearer $AGENT_RUNTIME_TOKEN"
```

Flags: `-config`, `-check`, `-chat <agent>`, `-run <agent> -input "..."`
(or stdin), `-version`.

## Files

Every tenant has its own file storage (in the store, so Postgres with the
postgres backend: shared by all replicas and covered by its backups).

- Agents use it with `tools: ["files.*"]`: `files__list`, `files__read`
  (text, in pieces for long files), `files__write` (create, overwrite or
  append), `files__delete`. Grant only reading with
  `tools: ["files.list", "files.read"]`.
- The API uploads and downloads raw bytes, so users can hand agents
  documents and fetch their results:

  ```bash
  curl -X PUT $RT/v1/files/kunden/liste.csv -H "Authorization: Bearer $KEY" --data-binary @liste.csv
  curl $RT/v1/files?prefix=reports/ -H "Authorization: Bearer $KEY"
  curl $RT/v1/files/reports/2026-09.md -H "Authorization: Bearer $KEY"
  ```

- Plans cap it: `max_files`, `max_file_mb`, `max_storage_mb` (defaults 500,
  5 and 50; `-1` = unlimited). The quota is checked in the same transaction
  as the write, so parallel uploads cannot overshoot it. `GET /v1/me` reports
  usage.
- Paths are relative (`reports/2026-09.md`); `..`, absolute paths and
  control characters are refused. Downloads are always sent as attachments
  with `nosniff`, never rendered on the API origin.

In multi-tenant mode there is deliberately no shell and no host file
system: agents of many tenants share one process.

## Shell

On a machine of your own (a home server, a dev box) an agent may run
commands through a `shell` connector. You decide in the config what is
allowed. Full example: `examples/homelab.yaml`.

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
  (`sudo x` is caught, `s""udo x` is not). With `allow` there is no shell:
  the line is split into words, the program must match `allow` by name
  (`/bin/rm` does not match `rm`), and `;`, `|` and `$(...)` stay literal
  arguments. Programs that start others (`env`, `xargs`, `find -exec`,
  `git -c`) reopen the door, so keep them off the list when that matters.
- **Environment.** Commands get only `PATH`, `HOME`, `USER`, `LANG`, `TZ`
  and friends plus `env`; agentd's own API keys and tokens stay out unless
  `inherit_env: true`.
- **Limits.** Per command `timeout_seconds` and `max_output` bytes per
  stream; set the agent's `tool_timeout_seconds` above the connector timeout.
- **Not a sandbox.** Commands run with agentd's user rights. Run agentd as
  a dedicated unprivileged user, or in a container or VM, when the machine
  matters.
- **Never multi-tenant.** agentd refuses the connector when
  `tenancy.enabled` is true (a child can read the parent's environment via
  `/proc`) and refuses it in every tenant document regardless of plan.

## Email agents

An `imap` trigger polls the mailbox (default every minute). For each unread
message it runs the agent with the sender, subject and plain-text body as
input, and sends the final answer as a threaded reply (`In-Reply-To`,
`References`, `Auto-Submitted: auto-replied`). Then it marks the message as
read. Loop protection:

- Bounces, auto-replies, list mail and `noreply` senders are skipped.
- The agent can answer `NO_REPLY` to stay silent.
- `allow_from` restricts senders; `allow_recipients` on the connector
  restricts who `send_email` may write to.
- A failing message is retried up to 3 times, then marked read.

Replies in the same thread continue the same agent session.

## Deploy

Everything is configured through env vars (see [`agents.yaml`](agents.yaml)).
Multi-tenant mode needs `TENANCY_ENABLED=true`, `DATABASE_URL` (Postgres),
`RUNTIME_SECRET_KEY` (`openssl rand -base64 32`; keep it safe, it decrypts
tenant secrets) and `AGENT_RUNTIME_TOKEN` (admin). Optionally set
`LLM_API_KEY` plus `TENANT_SHARED_LLM=true` to offer an operator-paid model
to tenants. Scale replicas for throughput, not per agent.

| Target | File |
|---|---|
| Docker Compose (runtime + Postgres) | [`deploy/docker-compose.yaml`](deploy/docker-compose.yaml) |
| Kubernetes (2 replicas, hardened pod) | [`deploy/kubernetes/agentd.yaml`](deploy/kubernetes/agentd.yaml) |
| Cloud Native Buildpacks (`pack`, kpack, Heroku-style PaaS) | `go.mod` at the root + [`project.toml`](project.toml); listens on `$PORT` |
| ITDone Cloud (as a customer app) | [`deploy/itdone/itdone.yaml`](deploy/itdone/itdone.yaml) |
| ITDone's own agent runtime | ArgoCD, `infra/k8s/envs/itdone-cloud/itdone-agentd/` in itdone-cloud; image built by [`.github/workflows/image.yml`](.github/workflows/image.yml) |

For a single-tenant setup, leave `TENANCY_ENABLED` unset; no database is
needed. For custom configs, mount a file (`-config`, `AGENT_CONFIG`) or pass
the YAML in `AGENT_CONFIG_YAML`.

The `Dockerfile` builds a distroless, non-root image of about 19 MB. `GOMEMLIMIT` (default `48MiB` in the image) is a soft heap
cap. Raise it if you run many concurrent agents with large contexts.

## Security notes

- Tool calls run with per-tool timeouts, output truncation, panic recovery
  and a per-run step limit (`max_steps`, default 12).
- `http` connectors send their configured credentials only to their own
  `base_url` host. Model-chosen URLs must match `allow_hosts` and cannot
  reach private or loopback addresses unless `allow_private: true`. The same
  check applies to redirects.
- SMTP header values are sanitized; STARTTLS is required unless
  `tls: none`.
- Hide trigger-only agents from the APIs with `expose: false`.

## Claude specifics

Default model is `claude-opus-5`. Assistant turns are replayed byte for byte
(including thinking blocks), so multi-step tool loops remain valid. The
runtime enables server-side refusal fallbacks by default
(`fallbacks: default`). Set `fallbacks: off` on the provider to disable them,
for example when the provider points at a proxy that rejects the parameter.
`effort` maps to `output_config.effort`.

## Development

```bash
make test    # unit + end-to-end tests (fake LLM, MCP, IMAP and SMTP servers)
make race    # with the race detector
make lint
```

Layout: `internal/llm` (providers), `internal/mcp` (client + server),
`internal/mail` (IMAP, SMTP, MIME), `internal/tools` (connectors),
`internal/agent` (loop, sessions), `internal/triggers` (heap scheduler,
cron, IMAP), `internal/tenant` (runtime cache, quotas, secrets, auth),
`internal/store` (memory/file and Postgres), `internal/netguard` (SSRF
guard), `internal/server` (HTTP, SSE, OpenAI, MCP, webhooks, tenant and
admin API), `internal/config`.

Postgres tests run when `AR_TEST_DATABASE_URL` is set:

```bash
docker run -d --rm --name ar-pg -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17-alpine
AR_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/postgres?sslmode=disable" make race
```
