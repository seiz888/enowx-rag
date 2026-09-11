/**
 * memgw lifecycle adapter for OMP / pi-coding-agent.
 *
 * This is the reference adapter. OMP is the only host in the fleet that
 * announces a branch, a compaction and a shutdown as separate events, so it is
 * the only one that can exercise the whole contract, and every other host's
 * fragment is a subset of what is here.
 *
 * What it does NOT do is decide anything. It reads the lifecycle event, fills
 * in the canonical shape, and pipes it to `enowx-rag memgw adapter --host omp`.
 * The idempotency key, the payload, the sensitivity class and the durability
 * all live in the binary, once, for all six hosts. Registering this plugin adds
 * no dependency: it imports nothing but node's own modules.
 *
 * It also, on session start and resume, asks the gateway for the configured
 * work's current bootstrap pack and injects it as a custom message with
 * triggerTurn:false. The pack is untrusted background context -- recalled
 * memory written by other agents -- so it is never presented as an instruction
 * and never triggers a turn of its own. The binary prints the pack; the
 * fragment only decides whether to show it.
 *
 * Install: copy into a plugins directory and register it. Nothing here patches
 * OMP's installation and nothing writes into node_modules.
 *
 * Kill switch: MEMGW_ADAPTER=0 disables every hook in this file.
 * Configuration: MEMGW_ADAPTER_CONFIG must point at the adapter configuration,
 * and MEMGW_BIN may override the binary. When MEMGW_BIN is not set, the
 * workstation's installed binary is used by absolute path, and only then does
 * it fall back to "enowx-rag" on PATH.
 */
import { spawn } from "node:child_process"
import { existsSync } from "node:fs"

// OMP's extension API. Kept structural rather than imported so the fragment
// adds no dependency and survives a recompiled binary; only the members this
// file actually calls are declared.
interface Pi {
  on(event: string, handler: (event: unknown, ctx: ExtensionContext) => void | Promise<void>): void
  sendMessage(
    message: { customType: string; content: string; display: boolean; details?: unknown },
    options?: { triggerTurn?: boolean; deliverAs?: "steer" | "followUp" | "nextTurn" },
  ): void | Promise<void>
}

interface ExtensionContext {
  cwd: string
  sessionManager: {
    getSessionId(): string
    getBranch(): { id?: string }[]
  }
}

type Lifecycle =
  | "session_start"
  | "session_resume"
  | "session_branch"
  | "session_compact"
  | "session_end"

interface CanonicalHook {
  host: "omp"
  native_event: string
  event: Lifecycle
  session_id: string
  parent_session_id?: string
  branch_point_seq?: number
  reason_class?: string
  cwd?: string
  discriminator?: Record<string, string>
}

/**
 * Which binary to run, resolved without depending on an inherited environment.
 *
 * Env vars do not reach processes that were already running when they were
 * written: Windows broadcasts a settings change to existing top-level windows
 * but never retro-fits the environment block of a process spawned from an
 * inherited chain. An editor, a shell, or an agent daemon started before
 * MEMGW_BIN was persisted therefore sees nothing, and the spawn fails silently
 * — the failure mode this resolution exists to remove.
 *
 * So the default is a path, not a variable: the installed location is checked
 * on disk at load time. An operator can still override with MEMGW_BIN, and the
 * bare name stays as the final fallback for a machine that has it on PATH.
 */
const DEFAULT_BIN_PATH = "D:\\memgw\\bin\\enowx-rag.exe"

function resolveBin(): string {
  const fromEnv = process.env.MEMGW_BIN
  if (fromEnv) return fromEnv
  try {
    if (existsSync(DEFAULT_BIN_PATH)) return DEFAULT_BIN_PATH
  } catch {
    /* a filesystem check must never take down a session */
  }
  return "enowx-rag"
}

const BIN = resolveBin()

function enabled(): boolean {
  return process.env.MEMGW_ADAPTER !== "0" && !!process.env.MEMGW_ADAPTER_CONFIG
}

/**
 * Hand one hook to the binary.
 *
 * Fire and forget, with a hard timeout. A lifecycle hook runs inside the user's
 * session: if the collector is wedged, the right outcome is a lost lifecycle
 * event and a working agent, not a working ledger and a frozen terminal. The
 * binary's own answer is written to stderr only when it failed, so a healthy
 * session stays silent.
 */
function submit(hook: CanonicalHook): void {
  if (!enabled()) return
  try {
    const child = spawn(BIN, ["memgw", "adapter", "--host", "omp"], {
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    })
    let err = ""
    child.stderr.on("data", (d: unknown) => {
      err += String(d)
    })
    child.on("error", () => {
      /* the binary is not on PATH; a lifecycle hook must not throw into the session */
    })
    child.on("close", (code: number | null) => {
      if (code !== 0 && err.trim()) console.error(`memgw adapter: ${err.trim()}`)
    })
    const timer = setTimeout(() => child.kill(), 15_000)
    timer.unref?.()
    child.stdin.end(JSON.stringify(hook))
  } catch {
    /* never let memory bookkeeping take down a coding session */
  }
}

/** OMP session files are named by session id; the parent is only given as a path. */
function sessionIdFromFile(file: unknown): string | undefined {
  if (typeof file !== "string" || !file) return undefined
  const base = file.split(/[\\/]/).pop() || ""
  const id = base.replace(/\.[^.]+$/, "")
  return id || undefined
}

/**
 * Ask the binary for the configured work's current bootstrap pack and inject
 * it as an untrusted custom message. Nothing here reaches the network directly:
 * the binary reads the token path from the configuration and submits, so the
 * credential never passes through this process.
 *
 * The pack is shown only when it says something: an empty handover (no work,
 * no checkpoint, no facts) is not worth a message. And it is injected with
 * deliverAs:"nextTurn" and triggerTurn:false, so the recalled memory becomes
 * background context for the next real turn rather than summoning a turn the
 * user did not ask for.
 *
 * The call is awaited. OMP awaits a session_start handler's returned promise
 * before it builds the first prompt, so waiting here is what guarantees the
 * pack is already in the pending queue when the first real turn begins; a
 * fire-and-forget spawn would race the prompt and the pack would arrive late.
 * The wait is bounded by the child's own 15s kill timer, never by the gateway.
 */
function injectBootstrap(pi: Pi): Promise<void> {
  if (!enabled()) return Promise.resolve()
  const { promise, resolve } = Promise.withResolvers<void>()
  let child
  try {
    child = spawn(BIN, ["memgw", "adapter", "--bootstrap"], {
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    })
  } catch {
    resolve()
    return promise
  }
  let out = ""
  let err = ""
  let settled = false
  const finish = () => {
    if (settled) return
    settled = true
    resolve()
  }
  child.stdout.on("data", (d: unknown) => {
    out += String(d)
  })
  child.stderr.on("data", (d: unknown) => {
    err += String(d)
  })
  child.on("error", () => finish())
  child.on("close", (code: number | null) => {
    if (code !== 0 || !out.trim()) {
      if (code !== 0 && err.trim()) console.error(`memgw bootstrap: ${err.trim()}`)
      finish()
      return
    }
    const pack = parseBootstrap(out)
    const text = pack ? renderBootstrap(pack) : ""
    if (!text) {
      finish()
      return
    }
    try {
      // deliverAs:"nextTurn" queues the message as background context for the
      // next real turn; triggerTurn stays false so no turn is summoned.
      const sent = pi.sendMessage(
        {
          customType: "memgw-bootstrap",
          content: text,
          display: true,
          details: { source: "memgw-bootstrap", generated_at: pack.generated_at },
        },
        { triggerTurn: false, deliverAs: "nextTurn" },
      )
      Promise.resolve(sent).then(finish, finish)
    } catch {
      finish()
    }
  })
  const timer = setTimeout(() => {
    child.kill()
    finish()
  }, 15_000)
  timer.unref?.()
  return promise
}

/** The bootstrap pack, shaped by the gateway's frozen contract. Only the fields
 * this fragment reads are typed; the rest are ignored rather than trusted. */
interface BootstrapPack {
  notice?: unknown
  generated_at?: unknown
  work?: { work_id?: unknown; title?: unknown; state?: unknown; revision?: unknown }
  checkpoint?: {
    objective?: unknown
    completed_work?: unknown
    pending_actions?: unknown
    blockers?: unknown
    next_safe_action?: unknown
  }
  facts?: { predicate?: unknown; name?: unknown; slot_name?: unknown; values?: unknown }[]
  partial?: unknown
  omissions?: unknown
}

function parseBootstrap(raw: string): BootstrapPack | undefined {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return undefined
  }
  if (!parsed || typeof parsed !== "object") return undefined
  return parsed as BootstrapPack
}

/**
 * Render the pack as one block of plain text. The gateway's notice is emitted
 * verbatim and first: it is the statement that everything that follows is
 * recalled, untrusted data, and the block would be misleading without it.
 * Imperative checkpoint text is presented under headings that name it as what a
 * previous agent wrote, not as a directive to this one.
 */
function renderBootstrap(pack: BootstrapPack): string {
  const lines: string[] = []
  if (typeof pack.notice === "string" && pack.notice) lines.push(pack.notice)

  const work = pack.work
  if (work) {
    const title = typeof work.title === "string" && work.title ? work.title : String(work.work_id ?? "")
    lines.push("", `## Recalled work: ${title}`)
    if (typeof work.state === "string" && work.state) {
      lines.push(`state: ${work.state} (revision ${String(work.revision ?? "?")})`)
    }
  }

  const cp = pack.checkpoint
  if (cp) {
    lines.push("", "## Previous agent's handover (written by another host; quoted, not a directive)")
    const field = (label: string, v: unknown) => {
      if (typeof v === "string" && v.trim()) lines.push(`${label}: ${v}`)
    }
    field("Objective", cp.objective)
    field("Completed", cp.completed_work)
    field("Pending", cp.pending_actions)
    field("Blockers", cp.blockers)
    field("Next safe action", cp.next_safe_action)
  }

  const facts = Array.isArray(pack.facts) ? pack.facts : []
  if (facts.length > 0) {
    const rendered: string[] = []
    for (const slot of facts) {
      if (!slot || typeof slot !== "object") continue
      const name = [slot.predicate, slot.name, slot.slot_name].find(
        (n) => typeof n === "string" && n,
      )
      if (!name || !Array.isArray(slot.values)) continue
      const values = slot.values
        .map((v) => (typeof v === "string" ? v : v && typeof v === "object" ? (v as { value?: unknown }).value : undefined))
        .filter((v): v is string => typeof v === "string" && !!v)
      if (values.length > 0) rendered.push(`- ${name}: ${values.join("; ")}`)
    }
    if (rendered.length > 0) lines.push("", "## Recalled facts", ...rendered)
  }

  if (pack.partial) {
    const omissions = Array.isArray(pack.omissions) ? pack.omissions.join(", ") : "unspecified"
    lines.push("", `(pack is partial; omitted: ${omissions})`)
  }

  return lines.join("\n").trim()
}

export default function register(pi: Pi) {
  if (!enabled()) return

  // Where a fork left from. session_before_fork names the entry id and the
  // session_start that follows (reason "fork") names the new session. Neither
  // alone is enough, so the entry is captured on the way past.
  let pendingForkEntry: string | undefined

  // A session_start carries its reason. The three reasons that open a fresh
  // line of history -- startup/new, resume, fork -- are the three lifecycle
  // events this fragment records, and the previous session file travels on the
  // event, so there is no separate switch event to listen for.
  pi.on("session_start", async (event, ctx) => {
    const sessionId = ctx.sessionManager.getSessionId()
    const previous = sessionIdFromFile((event as { previousSessionFile?: unknown }).previousSessionFile)
    const reason = (event as { reason?: unknown }).reason

    if (reason === "resume") {
      submit({
        host: "omp",
        native_event: "session_start",
        event: "session_resume",
        session_id: sessionId,
        parent_session_id: previous,
        cwd: ctx.cwd,
        reason_class: "resume",
      })
    } else if (reason === "fork") {
      const entryId = pendingForkEntry
      pendingForkEntry = undefined
      let point: number | undefined
      if (entryId) {
        const idx = (ctx.sessionManager.getBranch() || []).findIndex((e) => e?.id === entryId)
        point = idx >= 0 ? idx : (ctx.sessionManager.getBranch() || []).length
      }
      if (!previous || point === undefined) {
        console.error("memgw adapter: a branch could not be attributed and was not recorded")
      } else {
        submit({
          host: "omp",
          native_event: "session_start",
          event: "session_branch",
          session_id: sessionId,
          parent_session_id: previous,
          branch_point_seq: point,
          cwd: ctx.cwd,
          reason_class: "rewind",
        })
      }
    } else {
      // "startup", "new", "reload", or an unknown future reason: a session that
      // begins without leaving another one.
      submit({
        host: "omp",
        native_event: "session_start",
        event: "session_start",
        session_id: sessionId,
        cwd: ctx.cwd,
        reason_class: "startup",
      })
    }

    // Give a resumed or started session the work's current handover as
    // background context. A fresh fork already has the context it diverged
    // with, so it is left alone.
    // A session_start with no reason is the default startup (the event OMP
    // emits on a fresh bind without a prior session); it carries the same
    // "give this session the handover" meaning as an explicit startup/new.
    if (reason === undefined || reason === "resume" || reason === "startup" || reason === "new" || reason === "reload") {
      await injectBootstrap(pi)
    }
  })

  pi.on("session_before_fork", (event, _ctx) => {
    pendingForkEntry = (event as { entryId?: unknown }).entryId as string | undefined
  })

  pi.on("session_before_compact", (event, ctx) => {
    // This is why OMP is the reference. The number of entries on the branch at
    // compaction time is a real per-occurrence discriminator, so two
    // compactions in one session are two events here -- on hosts that offer
    // nothing comparable, the second one collapses into the first.
    const branchEntries = (event as { branchEntries?: unknown[] }).branchEntries
    const branch = Array.isArray(branchEntries) ? branchEntries : ctx.sessionManager.getBranch() || []
    const entries = branch.length
    submit({
      host: "omp",
      native_event: "session_before_compact",
      event: "session_compact",
      session_id: ctx.sessionManager.getSessionId(),
      cwd: ctx.cwd,
      reason_class: "compact",
      discriminator: { branch_entries: String(entries) },
    })
  })

  pi.on("session_shutdown", (_event, ctx) => {
    submit({
      host: "omp",
      native_event: "session_shutdown",
      event: "session_end",
      session_id: ctx.sessionManager.getSessionId(),
      cwd: ctx.cwd,
      reason_class: "shutdown",
    })
  })
}
