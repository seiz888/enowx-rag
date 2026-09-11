# memgw lifecycle adapters

One implementation, six registrations.

Everything that decides anything -- how a host event maps to a canonical event,
how the idempotency key is derived, what the payload looks like, where the event
goes and whether it is durable -- lives in the `enowx-rag` binary, in
`pkg/memgw/adapter`. What lives here is the smallest thing each host needs in
order to invoke it:

| Host | Fragment | Shape |
|---|---|---|
| OMP / pi-coding-agent | `omp/memgw-hooks.ts` | plugin that shells out |
| OpenCode | `opencode/memgw-hooks.ts` | plugin that shells out |
| Claude Code | `claude/settings.hooks.json` | settings fragment |
| Droid (Factory) | `droid/settings.hooks.json` | settings fragment |
| Codex CLI | `codex/hooks.json` | hooks file |
| Hermes Agent | `hermes/memgw-hook.sh` | shell hook |

None of these patches a host's installation, writes into `node_modules`, or
installs a global binary. Each is a file you copy and a few lines you add to a
configuration you already own.

## Why one binary instead of six adapters

The six hosts differ in how they announce a lifecycle moment. They do not differ
in what the moment means, and everything after the meaning is identical work:
derive an identifier that survives a crash, build a submission, hand it to
something durable, report honestly. Written six times in three languages, that
is six chances to get the idempotency key wrong -- and the idempotency key is
the only thing standing between a crashed host and a duplicated history.

## Configuration

`adapter.example.json` is the shape. It holds **no secret**: the gateway
credential is named by a path, never by a value, and a configuration that tries
to carry a token fails to parse. Point `MEMGW_ADAPTER_CONFIG` at your copy.

`writer_epoch` is the epoch this host was admitted under. It is configured
rather than discovered, because an adapter has to keep working while the gateway
is unreachable. A stale epoch is refused by the gateway as
`writer_epoch_fenced`, which is the intended outcome: a fenced host finds out at
its next write instead of continuing to write.

Kill switch on every fragment: `MEMGW_ADAPTER=0`.

## Checkpoints, and why they are not lifecycle events

A lifecycle event says a session started, resumed, compacted or ended. It says
nothing about the work, which is what a second agent needs in order to continue.
That is the checkpoint, and the adapter deliberately never manufactures one from
a lifecycle moment.

Two things produce one instead:

- `memgw checkpoint --submit` records a handover a model authored. It is exact
  and it only happens when something asks for it.
- `memgw checkpoint --capture` records one from the task list the agent already
  keeps. Nothing is invented -- every line was written by the agent, in its own
  tool call -- and because the mapping is fixed, a hook that fires twice over an
  unchanged list records nothing the second time.

The Claude Code fragment wires capture to `UserPromptSubmit` (which only
remembers the objective), to `PostToolUse` on the task tools, and to `Stop`,
`PreCompact` and `SessionEnd` as last chances. Both task tools are read:
`TodoWrite`, which carries the whole list in one call, and the
`TaskCreate`/`TaskUpdate`/`TaskList` trio, which carries one change at a time.
Claude Code 2.1.267 ships the trio and no `TodoWrite` at all, so a capture keyed
on `TodoWrite` alone would record nothing on it.

The read side is `memgw adapter --bootstrap`: the OMP fragment runs it at
`session_start` and puts the rendered handover into the session as background
context, so the next agent begins knowing what the last one finished, what is
pending, what is blocking and what is safe to do next -- with nothing pasted by
hand.

## Where events go

On Windows, to the local collector over its named pipe. The collector answers
only once the event is committed to its encrypted spool, so the hook returns
knowing the event survives the machine dying.

Off Windows, straight to the gateway, because the collector is a Windows
service. An event submitted that way is durable only if the gateway answered.
**Hermes therefore has no offline durability.** That is a stated gap, not a
configuration mistake.

If a collector is configured but unreachable and a gateway is configured too,
the event goes to the gateway and the hook's output says so (`"via":"gateway"`
with a message). Degrading is acceptable; degrading silently is not.

## What each host can actually report

| Host | start | resume | branch | compact | end | notes |
|---|---|---|---|---|---|---|
| OMP | yes | yes | yes | yes | yes | the only host with all five |
| Claude Code | yes | yes | no | yes | yes | `Stop` is per-turn and is deliberately ignored |
| Droid | yes | yes | no | yes | yes | same hook shape as Claude Code |
| Codex | yes | no | no | yes | yes | event shape inferred, never observed firing |
| OpenCode | yes | no | no | yes | **no** | the plugin API has no exit event |
| Hermes | yes | no | no | yes | yes | no local durable queue |

Two absences worth repeating. OpenCode announces no exit, so `session.ended` is
never recorded on that host and nothing here fabricates one from idleness -- an
idle session is one that is still open. And Claude Code's `Stop` fires when the
assistant finishes a turn, many times per session; recording it as a session
ending would make the ledger claim dozens of sessions where there was one.

## The cost of a crash-stable key

The key is derived from the meaning of the event and nothing else: no clock, no
random source, no counter. So a hook that fires, reaches the spool, and fires
again after the machine loses power produces the same key both times, and the
ledger holds one event.

The price is that when a host offers nothing to tell two occurrences apart --
Claude Code announces every automatic compaction identically -- the second
collapses into the first. One compaction is recorded where two happened. OMP
supplies a real per-occurrence discriminator (the number of entries on the
branch), so it does not have this problem, and any host that can supply one
should.

That trade is deliberate. A history that under-reports a repeated moment is
recoverable; a history that invents a duplicate on every crash is not.

## Trying it without touching a real installation

`install-sandbox.ps1` writes the fragments into a directory you name and nowhere
else. It refuses to write into a real agent home. See its own comment for what
it will and will not do.

To see what a hook would produce without sending anything:

```
echo '{"session_id":"s1","hook_event_name":"PreCompact","trigger":"auto"}' | \
  enowx-rag memgw adapter --host claude --config ./adapter.json --dry-run
```
