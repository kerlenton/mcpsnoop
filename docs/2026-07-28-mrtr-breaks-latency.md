# MCP's new spec quietly breaks latency measurement for every observability tool

The [MCP 2026-07-28 revision](https://blog.modelcontextprotocol.io/posts/2026-07-28-release-candidate/) is being written up all over the place, and almost all of it is about three things. The removal of the initialize handshake, the Tasks extension, and MCP Apps. There is a fourth change, tucked into a sub-page about a request pattern, and it is the one that broke something I maintain.

It is called [Multi Round-Trip Requests, SEP-2322](https://modelcontextprotocol.io/specification/draft/basic/patterns/mrtr). On its own it reads like a transport detail. In practice it means that every tool measuring how long an MCP call took is now, quietly, wrong. Mine included, until I fixed it.

## What actually changed

Before this revision, when a server needed something from the client mid-operation, say an elicitation, a sampling request, or a roots listing, it opened its own request back to the client over a held-open SSE stream and waited for the answer.

That standalone channel is gone. MRTR removes it outright, and the spec calls this a breaking change. A related rule, [SEP-2260](https://blog.modelcontextprotocol.io/posts/2026-07-28-release-candidate/), says a server-initiated request may now only happen while the server is actively processing a client request. So instead of opening a free-standing request of its own, the server answers the client's request with an InputRequiredResult, a result whose resultType is [input_required](https://modelcontextprotocol.io/specification/draft/schema), carrying an inputRequests map of what it needs plus an opaque requestState blob. The client gathers the answers and re-issues the original request with inputResponses and the echoed requestState.

The detail the whole thing hangs on is in the spec, in plain language.

> The JSON-RPC id MUST be different between the initial request and the retry, as
> they are independent requests.

They have to be independent, because the entire point is that any stateless server replica can pick up the retry with no memory of the first leg. All the context rides in the payload.

## Why that breaks measurement

Every MCP observability tool I know of correlates a request with its response by JSON-RPC id. It is the only identifier you get, so it is what everyone keys on.

MRTR makes one logical operation span several request and response pairs, each with a different id. So a tool keying on id sees N separate calls where there was one operation, most of them ending in a result that is not really a result but an I-need-more-input placeholder. And the duration of each fragment excludes the part that usually matters most, the seconds the human spent answering the elicitation.

Here is the concrete shape. A tools/call comes back in ten milliseconds with an input request. The user takes five seconds to answer. One more second for the real result. Six seconds of wall clock, one logical operation.

Measured through id-only correlation, which is what everything did before accounting for MRTR, you get this.

```
calls reported     2      (it was one operation)
duration measured  1s     (the real answer took ~6s)
tool call count    2      (inflated)
```

The five seconds the user spent deciding vanish completely. On a server that uses elicitation for confirmations, think delete-these-three-files, that interval is exactly the thing you most want to see, and it is the thing that disappears.

Those numbers are not illustrative. They came out of running the proxy against a scripted MRTR exchange twice, once with plain id correlation and once with the fix. With the fix, one call, six seconds, one in the tool count. If you want to try it on real traffic rather than a fixture, the [beta SDKs](https://blog.modelcontextprotocol.io/posts/sdk-betas-2026-07-28/) already speak the new pattern.

## Correlating without a correlation id

The fix sounds trivial, treat the retry as a continuation of the operation it retries, but there is no shared id to hang it on. The spec deliberately makes the retry a separate request. So the link has to be inferred, and inferring a link between two requests is exactly the kind of thing that is easy to get subtly wrong.

There are only two honest signals. The first is the requestState blob. It is opaque, server-minted, and the client must echo it back byte for byte, so an exact match is conclusive on its own, because nothing else produces the same blob. The second is the set of keys in inputResponses, which must match the inputRequests the server just sent.

What held up was a two-tier match. When requestState is present, match on it, since that is airtight. When the server issued none, fall back to requiring that the method, the operation name, and the full set of answered keys all agree, and refuse to link at all when more than one in-flight operation fits.

That last rule is the important one. A wrong link is worse than no link. It silently attributes one operation's timing to another, and you cannot see that it happened. Leaving two calls unlinked is honest. Fusing the wrong two is a lie with no symptom.

## The bug I put in my own fix

Worth writing down, because it is the part I would want to read.

The correlation worked. The retry was recognized and pointed back at the original call. But two of the read paths, the call list and the per-tool summary, enumerate events, and the retry event still carried a pointer to that same call. So the operation got counted twice again, from the other direction. One operation, two calls, right back where I started.

It did not show up by reading the code. It showed up because a test asserted that one operation is one call and the number came back two. The fix was one guard in each read path, skipping the continuation. But I would not have known to write that guard without running the thing and watching the count come back wrong. The reason I trust the numbers above is that they are the output of a test, not a claim I reasoned my way to.

## The bigger pattern

Step back and the revision is doing something consistent. It is decoupling one logical operation from one request and response pair, and it does it in more than one place. MRTR does it by making you retry under a new id. The [Tasks extension](https://modelcontextprotocol.io/extensions/tasks/overview) does it another way, a tools/call can come back with a task handle instead of a result, and the real work happens behind tasks/get polls under entirely different ids. Different mechanism, same consequence. The operation and the request are no longer the same thing.

Both break the same buried assumption, that an operation is a request and its matching response, and essentially every observability tool was built on that assumption. If you maintain anything that watches MCP traffic, this is the thing to go audit before the spec lands.

## The tool

I ran into this because I maintain [mcpsnoop](https://github.com/kerlenton/mcpsnoop), a local proxy that sits in the pipe between an MCP client and server and shows the JSON-RPC traffic live. It is where the numbers came from, and it now correlates MRTR retries back to the operation they continue, so the exchange reads as one call with the full duration instead of a scatter of fragments. But the post is about the spec change and what it does to measurement in general. The tool is just how I tripped over it.
