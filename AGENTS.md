# Codex Gateway repository guidance

- Keep the v0 gateway Grace-only, loopback-only, and based on Grace's existing ChatGPT login.
- Add adapters as statically linked Go packages. A deployment that changes adapters must regenerate the catalog and verify a fresh Codex `model/list`.
- Do not write to the legacy ledger without a separately scoped requirement.
- Treat implementation and `--check --diff` previews as distinct from permission for a live Ansible deployment or a Grace Codex config cutover.
- Deploy only clean committed source. Preserve rollback of the binary, catalog, config, unit, and service state; verify zero-change convergence.
