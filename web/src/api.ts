// One fetch helper for every surface. Paths are made relative to the document so
// the same bundle works served at "/" (Vercel) or "/panel/" (a machine's local
// reverse proxy). A non-2xx throws with the server's own message.
export async function api<T = any>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path.replace(/^\//, ""), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  let data: any = {};
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) throw new Error(data.error || `That did not work (${res.status}).`);
  return data as T;
}

export interface ModelUsage {
  model: string; weighted: number;
  input: number; output: number; cacheCreation: number; cacheRead: number;
}
// Presence is whether a person is working (a metered request in the last few
// minutes), online and idle (a machine checked in lately) or offline, and the
// moment that rests on: the request, or the last check-in ("" never).
export interface Presence { state: "working" | "idle" | "offline"; at?: string }
export interface Subject { id: string; name: string; weighted: number; byModel: ModelUsage[]; presence?: Presence }
export interface Board {
  window: string; asOf: string; subjects: Subject[];
  // What people since removed from the panel ran in the period: metered, so it is
  // counted, though it has no card.
  removedPeople?: number;
}
// One bucket of a person's usage over time, and the series of them. requests is
// only present at the grains cut from individual requests (seconds, minutes).
export interface ClassUsage { tokens: number; weighted: number }
// The class fields are absent from a panel older than the split; read them as zero.
export interface SeriesPoint {
  t: string; weighted: number; tokens: number; requests?: number;
  input?: ClassUsage; output?: ClassUsage; cacheWrite?: ClassUsage; cacheRead?: ClassUsage;
}
// SessionUsage is one Claude Code session as the gateway metered it, by id.
export interface SessionUsage { id: string; first: string; last: string; usage: SeriesPoint }
export interface Telemetry { res: string; person?: string; bucketSeconds: number; from: string; until: string; points: SeriesPoint[] }
// One account on the usage board: its real rolling windows (Claude's own %;
// hasReading is false until the gateway has read the login once), and for the
// asked period who ran it, each with their model split, plus the account's totals.
export interface AccountUsage {
  accountId: string; name: string; fiveH: number; sevenD: number;
  fiveHReset?: string; sevenDReset?: string; updatedAt: string; hasReading: boolean;
  people: { id: string; name: string; weighted: number; byModel: ModelUsage[]; ofWeekly: number; presence?: Presence }[];
  weighted: number; byModel: ModelUsage[];
  // The part of the plan's week (0..1 of the plan) no metered request explains —
  // the login was used before the gateway had met it, or outside the gateway. It
  // belongs to nobody on the card. weekBasis says how the plan's size was learned
  // ("history": the largest of its watched weeks); "none" means no week has finished yet, so people carry
  // exact tokens and no percentage. meteredSince is when the gateway first saw
  // the login, whichever account IDs it has had.
  unaccounted: number; tokensPerWeek: number; weekBasis: string; meteredSince?: string;
  // Per-model weekly allowances. Claude meters the top models separately, so an
  // account can have half its week left and almost none of its Fable — which is
  // the limit people actually hit.
  models?: { label: string; percent: number; resetsAt?: string }[];
}
// A ceiling: a 0..1 share of the weekly window past which the subject is turned away.
export interface Limit { id: string; subjectType: string; subjectId: string; maxPercent: number }
export interface Account { id: string; name: string; email?: string; plan?: string; hasLogin: boolean; addedBy?: string; warning?: string; shared?: { shareId: string; personId: string; personName: string; expiresAt?: string }[] }
// version is the clawdh build the machine last reported, and what that build covers
// (`latest · 7b506ea — feat(usage): the board is per account`); a release stamps the
// subject, an ad-hoc build is just the tag. Absent for a machine still on a build
// older than version reporting, which is itself the news.
// updateError is why the machine could not move off that build, when it knows.
export interface Device { id: string; name: string; lastSeen?: string; remote?: boolean; version?: string; updateError?: string }
export interface Person { id: string; name: string; email?: string; can?: string[]; canUntil?: string[]; devices?: Device[]; presence?: Presence }
export interface Job {
  id: string; kind: string; params?: string; status: string; result?: string;
  requestedBy?: string; createdAt: string; resolvedAt?: string;
}
export interface Panel { accounts: Account[]; people: Person[]; activity: { at: string; who: string; what: string }[] }
