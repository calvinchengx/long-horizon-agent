// Small shared pieces: formatting, the status badge, the item progress bar.
import type { ComponentChildren } from "preact";
import { useEffect, useState } from "preact/hooks";
import type { MissionSummary } from "./api";

export const usd = (v: number) => (v >= 100 ? `$${v.toFixed(0)}` : `$${v.toFixed(2)}`);

export function ago(iso: string | null | undefined, now = Date.now()): string {
  if (!iso) return "never";
  const s = Math.max(0, (now - Date.parse(iso)) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

/** Re-render every ``ms`` so relative times stay true. */
export function useTick(ms = 30_000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}

const STATUS: Record<string, { tone: string; text: string }> = {
  RUNNING: { tone: "info", text: "running" },
  SLEEPING: { tone: "muted", text: "sleeping" },
  WAITING_ON_HUMAN: { tone: "warn", text: "waiting on you" },
  DEGRADED_PARK: { tone: "warn", text: "parked" },
  DONE: { tone: "ok", text: "done" },
  ABORTED: { tone: "bad", text: "aborted" },
  IMPOSSIBLE: { tone: "bad", text: "impossible" },
};

export function StatusBadge({ status }: { status: string }) {
  const s = STATUS[status] ?? { tone: "muted", text: status.toLowerCase() };
  return <span class={`badge tone-${s.tone}`}>{s.text}</span>;
}

export function Progress({ items }: { items: MissionSummary["items"] }) {
  if (!items) return <span class="dim">checklist not readable here</span>;
  const open = items.total - items.split;
  const pct = open ? Math.round((items.done / open) * 100) : 0;
  return (
    <span class="progress" title={`${items.done} of ${open} done, ${items.blocked} blocked`}>
      <span class="bar">
        <span class="fill" style={{ width: `${pct}%` }} />
        {items.blocked > 0 && (
          <span class="fill blocked" style={{ width: `${Math.round((items.blocked / open) * 100)}%` }} />
        )}
      </span>
      <span class="num">
        {items.done}/{open}
      </span>
    </span>
  );
}

export function Section({ title, aside, children }: { title: string; aside?: ComponentChildren; children: ComponentChildren }) {
  return (
    <section class="card">
      <header class="card-head">
        <h2>{title}</h2>
        {aside}
      </header>
      {children}
    </section>
  );
}
