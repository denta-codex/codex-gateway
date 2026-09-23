# Codex Gateway

Grace's local gateway for Codex subscription traffic and additional model providers. Native Responses requests are forwarded to Grace's existing ChatGPT subscription endpoint. The gateway keeps request bytes and streamed response bytes intact, forwards native WebSockets, and logs route, status, timing, and completed token counts without logging prompts or credentials. Unknown `/v1/*` routes are logged and forwarded to the matching subscription path. A model with an unregistered adapter namespace fails locally.

The gateway listens on `127.0.0.1:48766`. It uses Grace's existing `~/.codex/auth.json`; it does not create another login. Near access-token expiry, it asks Codex's app server to refresh the managed ChatGPT login and rereads the credential file. The model catalog is fetched from the subscription endpoint, merged with compiled adapters, and written atomically. The last good catalog stays in place if refresh fails. There are no ledger writes in v0.

## Source layout

| Path | Purpose |
| --- | --- |
| `gateway/` | Codex-facing HTTP and WebSocket routing, stream forwarding, telemetry |
| `internal/subscription/` | Grace ChatGPT login and credential refresh |
| `internal/catalog/` | Subscription roster fetch and merged catalog validation |
| `internal/verify/` | Fresh Codex app-server `model/list` deployment gate |
| `adapter/` | Go adapter contract |
| `adapters/<provider>/` | Provider-specific implementation; `adapters/chatgpt/` is the first live adapter |
| `app/` and `cmd/codex-gateway/` | Static registration and executable |
| `deploy/` | Grace inventory, service, transaction, and rollback |

Adapters are ordinary Go packages linked at build time. An adapter owns a namespace such as `modal`, declares Codex catalog metadata with slugs such as `modal/model-name`, and implements HTTP Responses translation. Register it in `cmd/codex-gateway/main.go` by passing its value to `app.Run(ctx, os.Args[1:], adapterValue)`. Registration affects both request routing and catalog generation. Add the implementation, tests, and registration in one commit; deploy that commit; regenerate the catalog; and verify a **fresh** Codex `model/list` before routing everyday Codex through it. The Ansible playbook performs the catalog refresh and fresh `model/list` gate on every deployment. The verifier checks every listable catalog model. A later unused provider can remain in source without being registered; it is then absent from the binary and catalog. There is no runtime adapter loader. Private adapters used for deployment should live in a private Git repository or private branch with an exact committed source revision; ignored deployment code would defeat release reproducibility.

The first adapter exposes `subscription-chatgpt/chatgpt` as **Subscription ChatGPT**, with Instant, Thinking Light/Standard/Extended/Heavy, and Pro Standard mapped to Codex efforts `none`, `low`, `medium`, `high`, `xhigh`, and `max`. It uses Grace's existing login and an isolated helper for ChatGPT's consumer conversation protocol. See [the adapter README](adapters/chatgpt/README.md) for the protocol boundary and current limits. Native subscription traffic remains byte preserving. Namespaced WebSocket requests are rejected until an adapter explicitly supports that transport.

## Local checks

From the repository root:

```sh
mise exec -- go test ./...
uv run --no-project adapters/chatgpt/test_stream.py
mise exec -- go build -o /tmp/codex-gateway ./cmd/codex-gateway
ansible-playbook --syntax-check deploy/codex-gateway.yml
ansible-inventory --graph
```

The Ansible playbook requires a clean committed Git tree, confirms Grace's hostname, user, architecture, and paths, then tests and builds that exact commit. Preview checks the pinned ChatGPT helper dependencies in a disposable uv cache, makes a disposable catalog candidate, and checks it with a fresh Codex app server. Preview does not change the installed gateway, Grace Codex config, or system service. Live deployment stages the helper dependencies in the release directory; runtime helper launches use that cache offline.

```sh
ansible-playbook deploy/codex-gateway.yml --limit grace --check --diff
ansible-playbook deploy/codex-gateway.yml --limit grace-agent --check --diff
```

After separate authorization for the live cutover:

```sh
ansible-playbook deploy/codex-gateway.yml --limit grace-agent
curl --fail http://127.0.0.1:48766/ready
codex debug models
```

The live transaction stages a binary tied to the source commit and a validated catalog, installs `codex-gateway.service`, waits for readiness, then puts `openai_base_url` and `model_catalog_json` in a marked block at the top of Grace's `~/.codex/config.toml`. It validates Codex catalog loading and writes `~/.local/state/codex-gateway/receipt.json`. On an activation failure, it restores the previous binary link, catalog, config, service unit, and service state, then fails. The recovery transaction is removed after verified restoration and retained only if restoration cannot be verified. Repeating an unchanged release should report zero changes.

Managed paths are `~/.local/share/codex-gateway/`, `~/.local/state/codex-gateway/`, the marked gateway block in `~/.codex/config.toml`, and `/etc/systemd/system/codex-gateway.service`. The playbook does not own other files in those parent directories. For deliberate rollback after a successful deployment, check out the previous committed release and deploy it; catalog regeneration and `model/list` validation still apply. Grace is the sole v0 target. XPS client routing and ledger changes are later decisions.
