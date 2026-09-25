# Subscription ChatGPT adapter

`subscription-chatgpt/chatgpt` adds consumer ChatGPT Instant, Thinking, and Pro as one Codex model with six effort choices. The gateway's native subscription route already speaks Responses. This adapter translates Codex Responses requests to ChatGPT's separate conversation protocol and translates text and local tool calls back to Responses events.

| Codex effort | ChatGPT mode | Backend effort |
| --- | --- | --- |
| `none` | Instant | none |
| `low` | Thinking | min |
| `medium` | Thinking | standard |
| `high` | Thinking | extended |
| `xhigh` | Thinking | max |
| `max` | Pro | standard |

The helper reads Grace's existing ChatGPT access token and account ID from stdin. It sends them only to `https://chatgpt.com`; a signed WebSocket handoff URL must remain on that host or a subdomain. The WebSocket uses its signed URL and does not receive the bearer header. The helper never writes credentials or prompts to disk or logs. It prepares the conversation, obtains any required proof token, sends inference once, then emits text to the Go adapter. A request canceled by Codex kills the helper. A request already dispatched is never replayed.

The upstream conversation protocol is undocumented and can change. The transport and text stream decoder were adapted from the previously tested OpenCodex ChatGPT adapter. `proof.py` is copied from [suphotP/chatgpt-api](https://github.com/suphotP/chatgpt-api) at commit `f998a6d83f324cb3187396dd7efced0c40f29601`; its MIT license is preserved in `UPSTREAM-LICENSE`. The pinned helper dependencies are `curl_cffi==0.16.3` and `websocket-client==1.9.2`. Ansible checks them in a disposable cache during preview and stages an offline cache with the release during deployment.

Current contract: text input, text output, canonical Responses events over the gateway's HTTP or WebSocket frontend, and local function or custom tool calls. Images, audio, files, hosted web search, and namespaced Responses compaction are unavailable. The model advertises a conservative 32,000 token context window; longer Pro continuations have not been validated. Mode availability is checked against Grace's live ChatGPT catalog on each request.

Focused checks:

```sh
go test ./adapters/chatgpt ./internal/catalog ./gateway
uv run --no-project adapters/chatgpt/test_stream.py
```

An opt-in live Instant probe reads Grace's existing login without modifying it:

```sh
CODEX_GATEWAY_LIVE_CHATGPT=1 go test ./adapters/chatgpt -run '^TestLiveInstantInference$' -count=1
```
