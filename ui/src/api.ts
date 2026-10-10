// The UI API client: spec/serve/openapi.json, types generated into api.gen.ts (`npm run gen`).
import type { components, paths } from "./api.gen";

type S = components["schemas"];
export type MissionSummary = S["MissionSummary"];
export type MissionDetail = S["MissionDetail"];
export type ChecklistItem = S["ChecklistItem"];
export type MissionEvent = S["MissionEvent"];
export type CostSummary = S["CostSummary"];
export type CostCall = S["CostCall"];
export type CostGroup = S["CostGroup"];
type Ok<P extends keyof paths, M extends "get" | "post"> = P extends keyof paths
  ? paths[P][M] extends { responses: { 200: { content: { "application/json": infer T } } } }
    ? T
    : never
  : never;
export type ItemDetail = Ok<"/api/v1/missions/{mission_id}/items/{item_id}", "get">;
export type ItemDiffs = Ok<"/api/v1/missions/{mission_id}/items/{item_id}/diffs", "get">;
export type Costs = Ok<"/api/v1/missions/{mission_id}/costs", "get">;
export type Gate = S["Gate"];
export type OpenGate = S["OpenGate"];
export type ChecklistEdit = S["ChecklistEdit"];
export type Health = S["Health"];

/** Every event kind's payload, by kind (spec/obs/mission_events.json). */
export type Payloads = {
  [K in keyof S as S[K] extends object ? K : never]: S[K];
};

/** The token for writes: the server puts it in the page it serves to a browser holding the cookie. */
const token = document.querySelector<HTMLMetaElement>('meta[name="lha-token"]')?.content ?? "";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(method: "GET" | "POST", path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (method === "POST") {
    headers["X-LHA-Token"] = token;
    headers["Content-Type"] = "application/json";
  }
  const resp = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  const data = resp.headers.get("content-type")?.includes("json") ? await resp.json() : null;
  if (!resp.ok) {
    const err = data?.error ?? { code: "http", message: `HTTP ${resp.status}` };
    throw new ApiError(resp.status, err.code, err.message);
  }
  return data as T;
}

const m = (id: string) => `/api/v1/missions/${encodeURIComponent(id)}`;

export const api = {
  health: () => request<Health>("GET", "/api/v1/health"),
  missions: (limit = 200) =>
    request<{ missions: MissionSummary[] }>("GET", `/api/v1/missions?limit=${limit}`),
  mission: (id: string) => request<MissionDetail>("GET", m(id)),
  items: (id: string) => request<{ items: ChecklistItem[] }>("GET", `${m(id)}/items`),
  item: (id: string, itemId: string, limit = 200) =>
    request<ItemDetail>("GET", `${m(id)}/items/${encodeURIComponent(itemId)}?limit=${limit}`),
  diffs: (id: string, itemId: string, limit = 200) =>
    request<ItemDiffs>(
      "GET",
      `${m(id)}/items/${encodeURIComponent(itemId)}/diffs?limit=${limit}`,
    ),
  events: (id: string, after = 0, limit = 1000) =>
    request<{ events: MissionEvent[]; next_after: number }>(
      "GET",
      `${m(id)}/events?after=${after}&limit=${limit}`,
    ),
  costs: (id: string, limit = 200) => request<Costs>("GET", `${m(id)}/costs?limit=${limit}`),
  gates: (id: string) => request<{ gates: Gate[] }>("GET", `${m(id)}/gates`),
  steer: (id: string, note: string) => request("POST", `${m(id)}/steer`, { note }),
  snooze: (id: string, seconds: number) => request("POST", `${m(id)}/snooze`, { seconds }),
  edit: (id: string, edits: ChecklistEdit[], by: string) =>
    request("POST", `${m(id)}/checklist-edits`, { edits, by }),
  decide: (id: string, decision: string, by: string) =>
    request("POST", `${m(id)}/decision`, { decision, by }),
  abort: (id: string) => request("POST", `${m(id)}/abort`, {}),
};

/** Follow the live stream: mission events (resumed by EventSource itself) and mission rows. */
export function follow(
  opts: { missionId?: string; after?: number },
  on: { event?: (e: MissionEvent) => void; mission?: (m: MissionSummary) => void },
): () => void {
  const params = new URLSearchParams();
  if (opts.missionId) params.set("mission_id", opts.missionId);
  if (opts.after !== undefined) params.set("after", String(opts.after));
  const source = new EventSource(`/api/v1/stream?${params}`);
  if (on.event) {
    const handler = on.event;
    source.addEventListener("mission_event", (e) => handler(JSON.parse((e as MessageEvent).data)));
  }
  if (on.mission) {
    const handler = on.mission;
    source.addEventListener("mission", (e) => handler(JSON.parse((e as MessageEvent).data)));
  }
  return () => source.close();
}
