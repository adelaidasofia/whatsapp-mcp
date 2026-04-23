# Why not a fork?

Short answer: the owner wanted full architectural control and a direct audit surface for every line of code that touches their WhatsApp. Forking adds an upstream that can drift, carry inherited choices you'd rather not make, or be compromised. Building direct on `whatsmeow` means one dependency, one protocol-maintenance front, and our own mental model all the way down.

## What we considered

Before building, we audited every actively-maintained personal WhatsApp MCP:

| Repo | Stars | Last release | Verdict |
|---|---|---|---|
| `lharries/whatsapp-mcp` | 5.5k | v0.0.1 (Apr 2025), unmaintained since | Canonical but stale; 73 open issues, 83 open PRs |
| `LukasHaas/whatsapp-mcp` | 0 | Fork, unreleased | Good enhancements (LID resolution, accent-insensitive search) but unproven |
| `verygoodplugins/whatsapp-mcp` | 22 | v0.2.0 (Apr 2026), actively maintained | Most complete, well-maintained; we read it as reference |
| `msaelices/whatsapp-mcp-server` | 25 | various | Uses GreenAPI (paid third-party service), not personal WhatsApp |

All four depend on `go.mau.fi/whatsmeow` (or would, if they targeted personal WhatsApp). The meaningful code is in whatsmeow, not in the MCP wrapper. Forking one of these gets you a wrapper you could have written from scratch in a week.

## The decision

Building direct on `whatsmeow` gives us:

1. **One maintenance front.** When `whatsmeow` patches a protocol break, we bump the dep and test. No rebase negotiations with an intermediate fork's divergent changes.

2. **Full audit surface.** Every line of code that reads our messages is ours. We read it, we own it. Trust posture matches the sensitivity of the data (personal WhatsApp, investor conversations, family, client deals).

3. **Architecture freedom.** SQL schema, REST API shape, tool naming, security posture, all decided by us based on our use case. We're not carrying another project's legacy choices.

4. **Clean diff story.** Upstream `whatsmeow` commits are the only external changes we need to review. Small, focused, protocol-level.

5. **No carry-over of inherited bugs.** Existing MCPs have bug queues. Forking means inheriting them. Building fresh means we only have our own bugs.

## What we gave up

1. **Boilerplate rewrite.** The Go bridge and Python MCP server structures were written from scratch. Other repos had already solved many of these shapes (REST API endpoints, SQLite schema, media handling). Reading their code as reference was allowed; importing their code was not.

2. **Battle-tested edge-case handling.** `verygoodplugins` in particular has handled `StreamReplaced` session recovery, collision-safe media filenames, and call history capture through real usage. We're implementing those patterns fresh and will hit the same edge cases again; ours will be reviewed against their implementations when they're ready.

## What we borrowed (ideas, not code)

- Two-component architecture (Go bridge + Python MCP). Pattern established by `lharries`, refined by everyone since. Fits our existing MCP ecosystem (all Python MCPs elsewhere in the stack).
- SQLite as the persistence layer. Simple, local, queryable, portable.
- QR-code auth via `whatsmeow`'s multidevice protocol. The only path that works for personal WhatsApp in 2026.
- Call history capture pattern from `verygoodplugins`. We re-implemented with our own schema.
- LID resolution + accent-insensitive search pattern from `LukasHaas`. We re-implemented with NFD normalization in our own schema.

## When to reconsider

If at some point a fork becomes the better architectural choice (e.g., `whatsmeow` changes API surface in a way that would require near-identical wrapper code across all projects), this decision is reversible. The MIT-licensed ecosystem makes both directions cheap.

For now: direct on `whatsmeow` is the right call.
