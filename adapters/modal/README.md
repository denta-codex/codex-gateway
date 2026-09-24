# Modal adapter

The Modal adapter lets Codex use the Responses API against the gateway while
Modal receives its OpenAI-compatible Chat Completions API. It is intentionally
an in-process, statically linked adapter rather than another local proxy.

The request path is:

```text
Codex Responses JSON
  -> normalized messages, tool catalog, and tool history
  -> Modal /v1/chat/completions streaming request
  -> Chat delta stream
  -> typed Responses SSE events or a collected Responses JSON object
```

The translation follows the same useful boundary as OpenCodex's `openai-chat`
adapter: instructions and input items become Chat messages; function, custom,
namespace, and `additional_tools` declarations become Chat functions; tool
results become `role=tool`; and `reasoning_content`, text, usage, and parallel
tool-call deltas are restored to Responses events. Namespace `functions` is
flattened. Other namespaces use stable `namespace__name` wire aliases and are
restored on output. Custom tools use a single `{input: string}` function
argument on the Chat side.

Malformed JSON arguments, undeclared tools, oversized frames, and streams that
end without `[DONE]` or a finish reason fail closed with `response.failed`.
The adapter retries a transport error, HTTP 429, or HTTP 5xx once only before a
successful upstream stream begins. It never replays a partially consumed stream.

## Credential

Production reads the first available private regular file from:

1. `MODAL_INFERENCE_TOKEN_FILE`
2. systemd's `$CREDENTIALS_DIRECTORY/modal-inference-token`
3. `~/.config/codex-gateway/modal.env`
4. the legacy `~/.config/codex-modal-proxy/credentials.env`

The Grace service uses option 2. Its source is the TPM/host-encrypted,
root-owned `/etc/credstore.encrypted/codex-gateway-modal`; systemd decrypts it
into the service's private credentials directory at startup.

The file must not be group- or world-readable. It may contain the combined
`wk-....ws-...` token directly, `MODAL_PROXY_TOKEN=...`, or the original pair:

```dotenv
WK_SECRET=wk-...
WS_SECRET=ws-...
```

The gateway does not accept a raw secret environment variable and does not
forward the caller's authorization header, cookies, or arbitrary headers to
Modal. A stable, opaque `Modal-Session-Id` is derived from the Codex session
headers without exposing the original identifier.

## Limits

The adapter requires self-contained input and rejects `previous_response_id`,
remote compaction triggers, encrypted compaction state, input audio/video/files,
and namespaced WebSockets. Local Codex compaction boundary markers are accepted.
Hosted OpenAI tools are omitted because Modal cannot execute them; local
function, custom, namespace, and deferred tools remain available.
