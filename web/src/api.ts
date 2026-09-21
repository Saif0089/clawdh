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
  model: string; weighted: number; costUsd: number;
  input: number; output: number; cacheCreation: number; cacheRead: number;
}
export interface Subject { id: string; name: string; weighted: number; costUsd: number; byModel: ModelUsage[]; }
export interface Board { window: string; asOf: string; subjects: Subject[] }
export interface Burn { window: string; asOf: string; buckets: { hour: string; weighted: number; costUsd: number }[] }
export interface AccountWindow {
  accountId: string; name: string; fiveH: number; sevenD: number;
  fiveHReset?: string; sevenDReset?: string; updatedAt: string;
}
export interface Limit { id: string; subjectType: string; subjectId: string; windowKind: string; maxWeighted?: number; maxCostUsd?: number }
export interface Account { id: string; name: string; email?: string; plan?: string; hasLogin: boolean; addedBy?: string; warning?: string; shared?: { shareId: string; personId: string; personName: string; expiresAt?: string }[] }
// version is the clawdh build the machine last reported (`main · 7b506ea`); absent for one
// still on a build older than version reporting, which is itself the news.
export interface Device { id: string; name: string; lastSeen?: string; remote?: boolean; version?: string }
export interface Person { id: string; name: string; email?: string; can?: string[]; canUntil?: string[]; devices?: Device[] }
export interface Job {
  id: string; kind: string; params?: string; status: string; result?: string;
  requestedBy?: string; createdAt: string; resolvedAt?: string;
}
export interface Panel { accounts: Account[]; people: Person[]; activity: { at: string; who: string; what: string }[] }
