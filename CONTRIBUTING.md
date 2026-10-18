# Contributing

Thanks for helping! Bug reports, docs fixes and small improvements are
always welcome. For larger changes (a new connector, provider or API),
please open an issue first so we can agree on the shape before you write
the code.

## Build and test

You need Go 1.25. Nothing else; Postgres is optional.

```bash
make build   # bin/fabrik
make test    # unit + end-to-end tests with fake LLM, MCP, IMAP and SMTP servers
make race    # the same with the race detector
make lint    # gofmt + go vet
```

The Postgres store tests run when `AR_TEST_DATABASE_URL` is set, see the
README under *Development*.

## Guidelines

- **Keep it small.** agent-fabrik has two dependencies (`yaml.v3`, `lib/pq`) and
  we'd like to keep it that way. Protocols are implemented on the standard
  library.
- **Multi-tenancy first.** Anything a tenant can configure must be safe when
  thousands of strangers share the process: go through the SSRF guard
  (`internal/netguard`), respect plan limits, never expose server env vars.
- **Tests.** New behavior comes with a test; bug fixes with a test that fails
  without the fix.
- **Docs.** Update `README.md` and `examples/full.yaml` when you add or change
  an option.
- **Commits.** One topic per pull request, a short imperative subject line
  ("Webhooks: rate-limit before the lookup").

Security issues go through [SECURITY.md](SECURITY.md), not public issues.

By contributing you agree that your contribution is licensed under the
[Apache License 2.0](LICENSE).
