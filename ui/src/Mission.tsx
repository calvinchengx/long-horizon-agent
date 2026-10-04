import { useCallback, useEffect, useMemo, useState } from "preact/hooks";
import {
  api,
  ApiError,
  follow,
  type ChecklistItem,
  type CostCall,
  type CostSummary,
  type Gate,
  type MissionDetail,
  type MissionEvent,
} from "./api";
import { describe } from "./events";
import { ago, Progress, Section, StatusBadge, usd, useTick } from "./ui";

/** Kinds shown only with "every step": they are frequent and rarely the point. */
const DETAIL_KINDS = new Set(["tool_call", "llm_turn", "session_progress", "code_map", "sandbox_egress"]);
const TIMELINE_ROWS = 400;
const TABS = ["timeline", "checklist", "gates", "costs"] as const;
type Tab = (typeof TABS)[number];

export function Mission({ id }: { id: string }) {
  const [mission, setMission] = useState<MissionDetail | null>(null);
  const [error, setError] = useState("");
  // The tab is in the URL (?tab=checklist), so a link can open it.
  const [tab, setTabState] = useState<Tab | null>(() => {
    const t = new URLSearchParams(location.search).get("tab");
    return TABS.includes(t as Tab) ? (t as Tab) : null;
  });
  const setTab = (t: Tab) => {
    setTabState(t);
    const url = new URL(location.href);
    url.searchParams.set("tab", t);
    history.replaceState(history.state, "", url);
  };
  const now = useTick();

  const reload = useCallback(() => {
    api.mission(id).then(setMission, (e) => setError(e instanceof ApiError && e.status === 404 ? "No such mission." : String(e.message)));
  }, [id]);
  useEffect(reload, [reload]);
  // The live state (Temporal's answers) is cached a few seconds server-side: refresh at that pace.
  useEffect(() => {
    const t = setInterval(reload, 5000);
    return () => clearInterval(t);
  }, [reload]);

  if (error) return <p class="error">{error}</p>;
  if (!mission) return <p class="dim">Loading…</p>;
  const live = mission.live;
  // A mission that recorded no events (or one from before they were recorded) opens on its checklist.
  const shown = tab ?? (mission.last_event ? "timeline" : "checklist");
  return (
    <div class="mission">
      <header class="mission-head">
        <div>
          <h1>{mission.title || mission.mission_id}</h1>
          <div class="sub">
            {mission.mission_id} · {mission.durable ? "durable" : "local run"}
            {mission.workdir && <> · <code title={mission.workdir}>{shortPath(mission.workdir)}</code></>}
          </div>
          {mission.description && (
            <details class="brief">
              <summary>Mission brief</summary>
              <p>{mission.description}</p>
            </details>
          )}
        </div>
        <div class="facts">
          <StatusBadge status={live?.status ?? mission.status} />
          <Progress items={mission.items} />
          <span class="fact">
            <b>{usd(mission.spend.known_usd)}</b> spent
            {mission.spend.unknown_cost_calls > 0 && <span class="dim"> +{mission.spend.unknown_cost_calls} unpriced</span>}
          </span>
          {live && <span class="fact"><b>{live.cycles}</b> cycles</span>}
          <span class="fact dim">updated {ago(mission.updated_at, now)}</span>
        </div>
      </header>

      {mission.live_error && (
        <p class="notice tone-warn">Live state unavailable: {mission.live_error}</p>
      )}
      {live?.gate && <GateBanner id={id} gate={live.gate} onDone={reload} />}
      {live?.open_question && !live.gate && <p class="notice tone-warn">Waiting on: {live.open_question}</p>}
      {live?.resume_at && <p class="notice">Sleeping until {new Date(live.resume_at).toLocaleString()}.</p>}

      {mission.durable && live && <Controls id={id} mission={mission} onDone={reload} />}

      <nav class="tabs">
        {TABS.map((t) => (
          <button class={shown === t ? "active" : ""} onClick={() => setTab(t)} key={t}>
            {t}
          </button>
        ))}
      </nav>
      {shown === "timeline" && <Timeline id={id} />}
      {shown === "checklist" && <Checklist id={id} />}
      {shown === "gates" && <Gates id={id} />}
      {shown === "costs" && <Costs id={id} />}
    </div>
  );
}

// --- who is acting (remembered in this browser) ------------------------------------------------
function useWho(): [string, (v: string) => void] {
  const [who, setWho] = useState(() => {
    try {
      return localStorage.getItem("lha-who") ?? "";
    } catch {
      return "";
    }
  });
  const save = (v: string) => {
    setWho(v);
    try {
      localStorage.setItem("lha-who", v);
    } catch {
      /* private window: not remembered */
    }
  };
  return [who, save];
}

function GateBanner({ id, gate, onDone }: { id: string; gate: NonNullable<NonNullable<MissionDetail["live"]>["gate"]>; onDone: () => void }) {
  const [who, setWho] = useWho();
  const [busy, setBusy] = useState("");
  const [result, setResult] = useState("");
  const decide = async (decision: string) => {
    setBusy(decision);
    setResult("");
    try {
      await api.decide(id, decision, who.trim());
      setResult(`Sent "${decision}".`);
      onDone();
    } catch (e) {
      setResult(String((e as Error).message));
    } finally {
      setBusy("");
    }
  };
  return (
    <section class="gate">
      <div class="gate-q">
        <span class="badge tone-warn">{gate.kind === "deadlock" ? "stuck" : "approval needed"}</span>
        <p><Inline text={gate.question} /></p>
        {gate.request && (
          <pre class="request">
            {gate.request.tool} {gate.request.arguments}
            {gate.request.reason && `\n# ${gate.request.reason}`}
          </pre>
        )}
        <p class="dim">
          Defaults to <b>{gate.default_action}</b> at {new Date(gate.deadline).toLocaleString()}
          {gate.recommended && <> · recommended: <b>{gate.recommended}</b></>}
        </p>
      </div>
      <div class="gate-act">
        <label>
          Your name <input value={who} onInput={(e) => setWho((e.target as HTMLInputElement).value)} placeholder="recorded on the gate" />
        </label>
        <div class="row">
          {gate.options.map((o) => (
            <button key={o} class={o === "approve" || o === "retry" ? "primary" : ""} disabled={!who.trim() || !!busy} onClick={() => decide(o)}>
              {busy === o ? "…" : o}
            </button>
          ))}
        </div>
        {result && <p class="dim">{result}</p>}
      </div>
    </section>
  );
}

function Controls({ id, mission, onDone }: { id: string; mission: MissionDetail; onDone: () => void }) {
  const [note, setNote] = useState("");
  const [msg, setMsg] = useState("");
  const [confirmAbort, setConfirmAbort] = useState(false);
  const run = async (what: string, call: () => Promise<unknown>) => {
    setMsg("");
    try {
      await call();
      setMsg(`${what}: sent.`);
      onDone();
    } catch (e) {
      setMsg(`${what}: ${(e as Error).message}`);
    }
  };
  const notes = mission.live?.steer_notes ?? [];
  return (
    <details class="card controls">
      <summary>Steer this mission</summary>
      <div class="controls-body">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (note.trim()) run("Steering note", () => api.steer(id, note)).then(() => setNote(""));
          }}
        >
          <label>
            Note for every following cycle
            <textarea rows={2} maxLength={2000} value={note} onInput={(e) => setNote((e.target as HTMLTextAreaElement).value)} />
          </label>
          <button type="submit" disabled={!note.trim()}>Add note</button>
        </form>
        {notes.length > 0 && (
          <ul class="notes">
            {notes.slice(-3).map((n, i) => <li key={i}>{n}</li>)}
          </ul>
        )}
        <div class="row">
          <span>Sleep before the next cycle:</span>
          <button onClick={() => run("Snooze", () => api.snooze(id, 3600))}>1 hour</button>
          <button onClick={() => run("Snooze", () => api.snooze(id, 4 * 3600))}>4 hours</button>
          <button onClick={() => run("Wake", () => api.snooze(id, 0))}>wake now</button>
        </div>
        <div class="row">
          {confirmAbort ? (
            <>
              <span>Abort cancels the workflow; committed work stays.</span>
              <button class="danger" onClick={() => run("Abort", () => api.abort(id))}>Abort mission</button>
              <button onClick={() => setConfirmAbort(false)}>Keep running</button>
            </>
          ) : (
            <button class="danger-outline" onClick={() => setConfirmAbort(true)}>Abort…</button>
          )}
        </div>
        {(mission.live?.pending_edits ?? 0) > 0 && <p class="dim">{mission.live?.pending_edits} checklist edit(s) apply before the next cycle.</p>}
        {msg && <p class="dim">{msg}</p>}
      </div>
    </details>
  );
}

// --- the live timeline ------------------------------------------------------------------------
function Timeline({ id }: { id: string }) {
  const [events, setEvents] = useState<MissionEvent[] | null>(null);
  const [detail, setDetail] = useState(false);
  const now = useTick();

  useEffect(() => {
    let stop = () => {};
    let cancelled = false;
    (async () => {
      const all: MissionEvent[] = [];
      let after = 0;
      for (;;) {
        const page = await api.events(id, after, 1000);
        all.push(...page.events);
        if (page.events.length < 1000 || cancelled) break;
        after = page.next_after;
      }
      if (cancelled) return;
      setEvents(all);
      const last = all.length ? all[all.length - 1].id : 0;
      stop = follow({ missionId: id, after: last }, { event: (e) => setEvents((list) => [...(list ?? []), e]) });
    })();
    return () => {
      cancelled = true;
      stop();
    };
  }, [id]);

  const shown = useMemo(() => {
    const list = (events ?? []).filter((e) => detail || !DETAIL_KINDS.has(e.kind));
    return list.slice(-TIMELINE_ROWS).reverse();
  }, [events, detail]);

  if (!events) return <p class="dim">Loading events…</p>;
  return (
    <Section
      title={`Timeline (${events.length} events)`}
      aside={
        <label class="toggle">
          <input type="checkbox" checked={detail} onChange={(e) => setDetail((e.target as HTMLInputElement).checked)} /> every step
        </label>
      }
    >
      {shown.length === 0 ? (
        <p class="empty">No events recorded yet.</p>
      ) : (
        <ol class="timeline">
          {shown.map((e) => {
            const d = describe(e);
            return (
              <li key={e.id} class={`tone-${d.tone}`}>
                <time title={e.ts}>{ago(e.ts, now)}</time>
                <span class="cycle">{e.cycle_id}</span>
                <span class="label">{d.label}</span>
                <span class="text">{d.text}</span>
              </li>
            );
          })}
        </ol>
      )}
    </Section>
  );
}

// --- checklist, gates, costs ----------------------------------------------------------------
/** The last two segments of a path (the full one is the title). */
function shortPath(path: string): string {
  const parts = path.split("/").filter(Boolean);
  return parts.length > 2 ? `…/${parts.slice(-2).join("/")}` : path;
}

/** Text with `backticked` spans shown as code (item descriptions are written that way). */
function Inline({ text }: { text: string }) {
  return <>{text.split("`").map((part, i) => (i % 2 ? <code key={i}>{part}</code> : part))}</>;
}

function Checklist({ id }: { id: string }) {
  const [items, setItems] = useState<ChecklistItem[] | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    api.items(id).then((r) => setItems(r.items), (e) => setError(String(e.message)));
  }, [id]);
  if (error) return <p class="notice">{error}</p>;
  if (!items) return <p class="dim">Loading checklist…</p>;
  return (
    <Section title={`Checklist (${items.length} items)`}>
      <ol class="items">
        {items.map((it) => (
          <li key={it.id} class={`item item-${it.status}`}>
            <span class="id">{it.id}</span>
            <span class={`status status-${it.status}`}>{it.status.replace("_", " ")}</span>
            <div class="body">
              <p><Inline text={it.description} /></p>
              <p class="dim">
                {[
                  it.attempts > 0 && `attempts ${it.attempts}`,
                  it.depends_on.length > 0 && `after ${it.depends_on.join(", ")}`,
                  it.witnesses.length > 0 && `witnesses: ${it.witnesses.join(", ")}`,
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </p>
              {it.last_failure && it.status !== "done" && (
                <details>
                  <summary>last failure</summary>
                  <pre>{it.last_failure}</pre>
                </details>
              )}
            </div>
          </li>
        ))}
      </ol>
    </Section>
  );
}

function Gates({ id }: { id: string }) {
  const [gates, setGates] = useState<Gate[] | null>(null);
  useEffect(() => {
    api.gates(id).then((r) => setGates(r.gates));
  }, [id]);
  if (!gates) return <p class="dim">Loading gates…</p>;
  return (
    <Section title={`Human gates (${gates.length})`}>
      {gates.length === 0 ? (
        <p class="empty">No gate has opened.</p>
      ) : (
        <table>
          <thead>
            <tr><th>Opened</th><th>Question</th><th>Status</th><th>Decision</th></tr>
          </thead>
          <tbody>
            {gates.map((g) => (
              <tr key={g.gate_id}>
                <td title={g.opened_at}>{new Date(g.opened_at).toLocaleString()}</td>
                <td>{g.question}</td>
                <td><span class={`badge tone-${g.status === "RESOLVED" ? "ok" : g.status === "DEFAULTED" ? "muted" : "warn"}`}>{g.status.toLowerCase()}</span></td>
                <td>{g.decision ? <>{g.decision}{g.resolved_by && <span class="dim"> by {g.resolved_by}</span>}</> : <span class="dim">—</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Section>
  );
}

function Costs({ id }: { id: string }) {
  const [data, setData] = useState<{ summary: CostSummary; calls: CostCall[] } | null>(null);
  useEffect(() => {
    api.costs(id, 500).then(setData);
  }, [id]);
  const byRole = useMemo(() => {
    const roles = new Map<string, { usd: number; calls: number }>();
    for (const c of data?.calls ?? []) {
      const r = roles.get(c.role || "lead") ?? { usd: 0, calls: 0 };
      r.usd += c.usd ?? 0;
      r.calls += 1;
      roles.set(c.role || "lead", r);
    }
    return [...roles.entries()].sort((a, b) => b[1].usd - a[1].usd);
  }, [data]);
  if (!data) return <p class="dim">Loading costs…</p>;
  const s = data.summary;
  return (
    <Section title="Spend">
      <p class="facts">
        <span class="fact"><b>{usd(s.known_usd)}</b> over {s.calls} calls</span>
        {s.unknown_cost_calls > 0 && <span class="fact">{s.unknown_cost_calls} calls of unknown cost</span>}
        <span class="fact dim">{s.input_tokens.toLocaleString()} tokens in · {s.output_tokens.toLocaleString()} out</span>
      </p>
      {byRole.length > 0 && (
        <p class="facts">
          {byRole.map(([role, r]) => (
            <span class="fact" key={role}>{role}: {usd(r.usd)} <span class="dim">({r.calls})</span></span>
          ))}
          {data.calls.length < s.calls && <span class="fact dim">(the latest {data.calls.length} calls)</span>}
        </p>
      )}
      <table>
        <thead>
          <tr><th>When</th><th>Cycle</th><th>Role</th><th>Model</th><th class="num">Tokens in/out</th><th class="num">Cost</th></tr>
        </thead>
        <tbody>
          {data.calls.map((c, i) => (
            <tr key={i}>
              <td title={c.ts}>{new Date(c.ts).toLocaleString()}</td>
              <td>{c.cycle_id}</td>
              <td>{c.role || "lead"}</td>
              <td>{c.model}</td>
              <td class="num">{c.input_tokens.toLocaleString()} / {c.output_tokens.toLocaleString()}</td>
              <td class="num">{c.usd == null ? <span class="dim">unknown</span> : usd(c.usd)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </Section>
  );
}
