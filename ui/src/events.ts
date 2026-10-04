// One readable line per recorded event (the kinds of spec/obs/mission_events.json). A kind this
// build does not know is shown by name, never dropped: the API only grows (spec/serve/README.md).
import type { MissionEvent, Payloads } from "./api";

export type Tone = "ok" | "bad" | "warn" | "info" | "muted";
export interface Described {
  label: string;
  text: string;
  tone: Tone;
}

const usd = (v: number | null | undefined) => (v == null ? "unpriced" : `$${v.toFixed(4)}`);
const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

type P<K extends keyof Payloads> = Payloads[K];

export function describe(e: MissionEvent): Described {
  const p = e.payload as Record<string, unknown>;
  switch (e.kind) {
    case "cycle_started":
      return { label: "cycle", text: `started item ${(p as P<"cycle_started">).item_id}`, tone: "info" };
    case "tool_call": {
      const t = p as P<"tool_call">;
      const who = t.role ? `${t.role}: ` : "";
      return t.ok
        ? { label: "tool", text: `${who}${t.tool}`, tone: "muted" }
        : { label: "tool", text: `${who}${t.tool} failed: ${firstLine(t.error)}`, tone: "warn" };
    }
    case "llm_turn": {
      const t = p as P<"llm_turn">;
      return { label: "model", text: `${t.model || "turn"} · ${t.output_tokens} tokens out`, tone: "muted" };
    }
    case "session_progress": {
      const t = p as P<"session_progress">;
      return {
        label: "session",
        text: `turn ${t.turns}, ${plural(t.tool_calls, "tool call")}${t.tool ? `, last ${t.tool}` : ""} · ${usd(t.spent_usd)} so far`,
        tone: "muted",
      };
    }
    case "claude_code_session": {
      const t = p as P<"claude_code_session">;
      return {
        label: "session",
        text: `ended after ${plural(t.turns, "turn")}, ${plural(t.tool_calls, "tool call")}${t.stopped ? ` (${t.stopped})` : ""}`,
        tone: t.stopped ? "warn" : "info",
      };
    }
    case "verify": {
      const t = p as P<"verify">;
      const failed = t.checks.filter((c) => !c.passed && c.gating).map((c) => c.name);
      const why = { done: "on done", tool: "asked by the session", cycle: "at cycle end" }[t.trigger];
      return {
        label: "verify",
        text: `${t.verdict} (${why})${failed.length ? `: ${failed.join(", ")} failed` : `: ${plural(t.checks.length, "check")}`}`,
        tone: t.verdict === "passed" ? "ok" : t.verdict === "failed" ? "bad" : "warn",
      };
    }
    case "checkpoint": {
      const t = p as P<"checkpoint">;
      return {
        label: "checkpoint",
        text: `${t.verdict} · ${t.head_sha.slice(0, 10)}`,
        tone: t.verified ? "ok" : t.verdict === "failed" ? "bad" : "warn",
      };
    }
    case "turns_exhausted":
      return { label: "cycle", text: `ran out of turns (${(p as P<"turns_exhausted">).max_turns})`, tone: "warn" };
    case "invalid_reply":
      return { label: "model", text: `invalid reply: ${(p as P<"invalid_reply">).reason}`, tone: "warn" };
    case "code_map": {
      const t = p as P<"code_map">;
      return { label: "code map", text: t.ok ? `${t.mode} map, ${t.bytes ?? 0} bytes` : `unavailable: ${firstLine(t.error)}`, tone: "muted" };
    }
    case "system_one": {
      const t = p as P<"system_one">;
      return { label: "triage", text: t.error ? `failed: ${t.error}` : `${t.answer} (${Math.round(t.confidence * 100)}%) → ${t.action}`, tone: t.action === "continue" ? "muted" : "warn" };
    }
    case "check_quarantined":
      return { label: "flaky", text: `quarantined ${(p as P<"check_quarantined">).check}`, tone: "warn" };
    case "quarantined_check_failed":
      return { label: "flaky", text: `${(p as P<"quarantined_check_failed">).check} failed every attempt`, tone: "bad" };
    case "sandbox_egress": {
      const t = p as P<"sandbox_egress">;
      return { label: "egress", text: `${t.decision} ${t.method} ${t.host}:${t.port} ×${t.count}`, tone: t.decision === "allow" ? "muted" : "warn" };
    }
    case "governor_block":
      return { label: "budget", text: String(p.reason ?? ""), tone: "bad" };
    case "deadlocked":
      return { label: "stuck", text: String(p.reason ?? ""), tone: "bad" };
    case "model_unavailable":
      return { label: "model", text: `unavailable: ${p.reason}`, tone: "bad" };
    case "decision_chain_invalid":
      return { label: "decisions", text: `log failed verification: ${p.reason}`, tone: "bad" };
    case "resumed": {
      const t = p as P<"resumed">;
      return { label: "resumed", text: `run ${t.run} from cycle ${t.cycle_offset}`, tone: "info" };
    }
    case "ownership_violation": {
      const t = p as P<"ownership_violation">;
      return { label: "ownership", text: `${t.writer} wrote outside its files: ${t.paths.join(", ")}`, tone: "warn" };
    }
    case "ownership_released":
      return { label: "ownership", text: `released ${(p as P<"ownership_released">).paths.join(", ") || "nothing"}`, tone: "muted" };
    case "lease": {
      const t = p as P<"lease">;
      return { label: "lease", text: `${t.writer} ${t.granted ? "got" : "denied"} ${t.path}${t.why ? `: ${t.why}` : ""}`, tone: t.granted ? "muted" : "warn" };
    }
    case "reflection":
      return { label: "reflection", text: `on ${p.item}`, tone: "muted" };
    case "skill_stored":
      return { label: "memory", text: "stored a skill", tone: "muted" };
    case "memory_reembedded": {
      const t = p as P<"memory_reembedded">;
      return { label: "memory", text: `re-embedded ${t.count} with ${t.embedding_model}`, tone: "muted" };
    }
    case "memory_consolidated": {
      const t = p as P<"memory_consolidated">;
      return { label: "memory", text: `consolidated ${t.episodes} episodes into ${t.facts} facts (${t.mode})`, tone: "muted" };
    }
    case "loop_detected":
      return { label: "loop", text: `item ${p.item_id} keeps failing`, tone: "bad" };
    case "review": {
      const t = p as P<"review">;
      return { label: "review", text: `${t.item}: ${t.verdict}`, tone: t.blocking ? "bad" : "ok" };
    }
    case "review_reopened":
      return { label: "review", text: `reopened ${p.item}`, tone: "warn" };
    case "review_screen": {
      const t = p as P<"review_screen">;
      return { label: "screen", text: `${t.item}: ${plural(t.findings, "finding")}${t.forced ? " (forced a review)" : ""}`, tone: t.findings ? "warn" : "muted" };
    }
    case "integration": {
      const t = p as P<"integration">;
      return { label: "merge", text: `${t.item} ${t.merged ? "merged" : `not merged: ${firstLine(t.reason)}`}`, tone: t.merged ? "ok" : "warn" };
    }
    case "parallel_wave":
      return { label: "wave", text: `items ${(p as P<"parallel_wave">).items.join(", ")} in parallel`, tone: "info" };
    case "research": {
      const t = p as P<"research">;
      return { label: "research", text: `${t.item}: ${plural(t.n, "brief")}${t.failed ? `, ${t.failed} failed` : ""}`, tone: "muted" };
    }
    case "research_failed":
      return { label: "research", text: `${p.item} failed: ${firstLine(String(p.error ?? ""))}`, tone: "warn" };
    case "memory_degraded":
    case "memory_error":
      return { label: "memory", text: String(p.reason ?? p.error ?? ""), tone: "warn" };
    default:
      return { label: e.kind.replace(/_/g, " "), text: summarize(p), tone: "muted" };
  }
}

function firstLine(text: string | undefined): string {
  const line = (text ?? "").trim().split("\n")[0] ?? "";
  return line.length > 160 ? `${line.slice(0, 157)}...` : line;
}

function summarize(p: Record<string, unknown>): string {
  return Object.entries(p)
    .filter(([, v]) => typeof v !== "object")
    .slice(0, 4)
    .map(([k, v]) => `${k}=${v}`)
    .join(" ");
}
