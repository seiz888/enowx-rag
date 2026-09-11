/**
 * memgw lifecycle adapter for OpenCode.
 *
 * A subset of the OMP fragment, because OpenCode's plugin API is a subset of
 * OMP's. Two gaps, both real and neither worked around:
 *
 *   - There is no exit or shutdown event. A session that is closed is never
 *     announced, so `session.ended` is never recorded on this host. The
 *     lifecycle a reader gets is "started, compacted, compacted" and then
 *     silence. Nothing here fabricates an ending from idleness: an idle
 *     session is one that is still open.
 *   - There is no branch event and no SessionStart event.
 *
 * What is recorded: session.created becomes a start, and the compaction hook
 * becomes a compaction. Two compactions in one session announce themselves
 * identically, so they collapse into one event -- see the note on Key in the
 * adapter package for why that is preferred over a key with a clock in it.
 *
 * The read half is wired too. OpenCode has no SessionStart, but its
 * `experimental.chat.system.transform` hook lets a plugin add text to the
 * system prompt, so the current work's handover is fetched once per session
 * and appended there: the same recalled-memory block OMP and Claude Code are
 * given, with the gateway's untrusted-context notice first.
 *
 * Kill switch: MEMGW_ADAPTER=0. Configuration: MEMGW_ADAPTER_CONFIG.
 */
import { spawn } from "node:child_process"

const BIN = process.env.MEMGW_BIN || "enowx-rag"

function enabled(): boolean {
  return process.env.MEMGW_ADAPTER !== "0" && !!process.env.MEMGW_ADAPTER_CONFIG
}

function submit(hook: Record<string, unknown>): void {
  if (!enabled()) return
  try {
    const child = spawn(BIN, ["memgw", "adapter", "--host", "opencode"], {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    })
    let err = ""
    child.stderr.on("data", (d: unknown) => {
      err += String(d)
    })
    child.on("error", () => {})
    child.on("close", (code: number | null) => {
      if (code !== 0 && err.trim()) console.error(`memgw adapter: ${err.trim()}`)
    })
    const timer = setTimeout(() => child.kill(), 15_000)
    timer.unref?.()
    child.stdin.end(JSON.stringify(hook))
  } catch {
    /* memory bookkeeping never takes down a coding session */
  }
}

/**
 * additionalContext reads the configured work's handover and returns the
 * assistant-facing text, or "" when there is none. It runs the same
 * `--bootstrap-context` command the Claude and OMP fragments use, so the
 * rendering (notice first, checkpoint under "quoted, not a directive") is
 * identical across hosts. Nothing is cached: a stale handover presented as
 * current is the failure this read exists to avoid.
 */
function additionalContext(): Promise<string> {
  const { promise, resolve } = Promise.withResolvers<string>()
  if (!enabled()) {
    resolve("")
    return promise
  }
  try {
    const child = spawn(BIN, ["memgw", "adapter", "--host", "opencode", "--bootstrap-context"], {
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    })
    let out = ""
    child.stdout.on("data", (d: unknown) => {
      out += String(d)
    })
    child.on("error", () => resolve(""))
    child.on("close", () => {
      resolve(readAdditionalContext(out))
    })
    const timer = setTimeout(() => child.kill(), 15_000)
    timer.unref?.()
  } catch {
    resolve("")
  }
  return promise
}

/**
 * readAdditionalContext pulls the assistant-facing text out of a
 * `--bootstrap-context` response. The response is host-shaped JSON, so it is
 * narrowed field by field rather than trusted: an unparseable or unexpected
 * answer yields "" and the session simply gets no recalled memory.
 */
function readAdditionalContext(raw: string): string {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return ""
  }
  if (!parsed || typeof parsed !== "object" || !("hookSpecificOutput" in parsed)) return ""
  const hookOutput = parsed.hookSpecificOutput
  if (!hookOutput || typeof hookOutput !== "object" || !("additionalContext" in hookOutput)) return ""
  const ctx = hookOutput.additionalContext
  return typeof ctx === "string" ? ctx : ""
}

const injected = new Set<string>()

export const MemgwLifecycle = async () => {
  return {
    // OpenCode has no SessionStart, so this is the injection point it does
    // offer. The handover is fetched once per session and appended to the
    // system prompt as background context.
    "experimental.chat.system.transform": async (input: { sessionID?: string }, output: { system: string[] }) => {
      const sid = input?.sessionID
      if (!sid || injected.has(sid)) return
      const block = await additionalContext()
      injected.add(sid)
      if (block) output.system.push(block)
    },

    event: async ({ event }: { event: { type?: string; properties?: { sessionID?: string; info?: { id?: string } } } }) => {
      if (event?.type !== "session.created") return
      const sid = event.properties?.sessionID || event.properties?.info?.id
      if (!sid) return
      submit({
        host: "opencode",
        native_event: "session.created",
        event: "session_start",
        session_id: String(sid),
        cwd: process.cwd(),
        reason_class: "startup",
      })
    },

    "experimental.session.compacting": async (input: { sessionID?: string; sessionId?: string }) => {
      const sid = input?.sessionID || input?.sessionId
      if (!sid) return
      submit({
        host: "opencode",
        native_event: "experimental.session.compacting",
        event: "session_compact",
        session_id: String(sid),
        cwd: process.cwd(),
        reason_class: "compact",
      })
    },
  }
}

export default MemgwLifecycle
