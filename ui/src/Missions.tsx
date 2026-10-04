import { useEffect, useState } from "preact/hooks";
import { api, follow, type MissionSummary } from "./api";
import { ago, Progress, StatusBadge, usd, useTick } from "./ui";

export function Missions() {
  const [missions, setMissions] = useState<MissionSummary[] | null>(null);
  const [error, setError] = useState("");
  const now = useTick();

  useEffect(() => {
    api.missions().then((r) => setMissions(r.missions), (e) => setError(String(e.message)));
    // A changed mission row arrives as a `mission` message: replace it, newest first.
    return follow(
      {},
      {
        mission: (m) =>
          setMissions((list) =>
            [m, ...(list ?? []).filter((x) => x.mission_id !== m.mission_id)].sort((a, b) =>
              b.updated_at.localeCompare(a.updated_at),
            ),
          ),
      },
    );
  }, []);

  if (error) return <p class="error">{error}</p>;
  if (!missions) return <p class="dim">Loading missions…</p>;
  if (!missions.length)
    return <p class="empty">No missions recorded yet. Start one with <code>lha mission</code> or <code>lha mission-start</code>.</p>;

  return (
    <table class="missions">
      <thead>
        <tr>
          <th>Mission</th>
          <th>Status</th>
          <th>Items</th>
          <th class="num">Spend</th>
          <th>Last activity</th>
        </tr>
      </thead>
      <tbody>
        {missions.map((m) => (
          <tr key={m.mission_id}>
            <td>
              <a href={`/missions/${encodeURIComponent(m.mission_id)}`} class="title">
                {m.title || m.mission_id}
              </a>
              <div class="sub">
                {m.mission_id}
                {m.durable && <span class="tag">durable</span>}
              </div>
            </td>
            <td>
              <StatusBadge status={m.status} />
            </td>
            <td>
              <Progress items={m.items} />
            </td>
            <td class="num">
              {usd(m.spend.known_usd)}
              {m.spend.unknown_cost_calls > 0 && (
                <span class="dim" title="calls whose cost is unknown">
                  {" "}+{m.spend.unknown_cost_calls}?
                </span>
              )}
            </td>
            <td>
              {m.last_event ? (
                <span title={m.last_event.ts}>
                  {ago(m.last_event.ts, now)} <span class="dim">· {m.last_event.kind.replace(/_/g, " ")}</span>
                </span>
              ) : (
                <span class="dim">{ago(m.updated_at, now)}</span>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
