# Routed web search

`websearch.Service` is an opt-in wrapper for a provider adapter's Responses
transport. It translates Codex's hosted `web_search` declaration into a normal
function for the routed model, executes calls through Grace's ChatGPT
subscription, and continues the routed turn with a bounded tool result.

An adapter creates the service after `SetSubscriptionAuth` and calls it from
`ServeResponses`:

```go
search, err := websearch.New(websearch.Config{Auth: auth})
// Keep construction errors as adapter initialization errors.

search.ServeResponses(w, r, body, websearch.BackendFunc(
    func(ctx context.Context, iteration []byte) (*http.Response, error) {
        return adapterResponsesRoundTrip(ctx, iteration)
    },
))
```

The backend callback must perform exactly one stateless Responses iteration
and return its HTTP response without writing to the client. It may capture
request-scoped provider headers or credentials, but must not receive or reuse
the ChatGPT token. The service handles both JSON and SSE Responses results.

Requests without a hosted search tool make one byte-preserving backend call.
Requests with search are buffered at model-iteration boundaries so synthetic
tool calls never escape to Codex. A mixed iteration containing a real client
tool is terminal: the real tool is returned and the synthetic search call is
removed.

Compiling this package does not advertise search. The consuming adapter must
set `supports_search_tool` and `web_search_tool_type` in its catalog rows only
after its integration tests pass.
