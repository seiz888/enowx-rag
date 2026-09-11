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
 *   - There is no branch event.
 *
 * What is recorded: session.created becomes a start, and the compaction hook
 * becomes a compaction. Two compactions in one session announce themselves
 * identically, so they collapse into one event -- see the note on Key in the
 * adapter package for why that is preferred over a key with a clock in it.
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
    const timer: any = setTimeout(() => child.kill(), 15_000)
    timer.unref?.()
    child.stdin.end(JSON.stringify(hook))
  } catch {
    /* memory bookkeeping never takes down a coding session */
  }
}

export const MemgwLifecycle = async () => {
  return {
    event: async ({ event }: { event: any }) => {
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

    "experimental.session.compacting": async (input: any) => {
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
