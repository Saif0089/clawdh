export function fmtNum(n: number): string {
  n = Math.round(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
  return String(n);
}

// modelLabel folds a dated model id into its family name. Models are named,
// never colour-coded: on the boards colour is reserved for a window's state.
const MODEL_FAMILIES: [RegExp, string][] = [
  [/fable|mythos/, "Fable / Mythos"], [/opus/, "Opus"], [/sonnet/, "Sonnet"], [/haiku/, "Haiku"], [/unknown/, "Unknown"],
];
export function modelLabel(m: string): string {
  const lm = (m || "").toLowerCase();
  for (const [test, label] of MODEL_FAMILIES) if (test.test(lm)) return label;
  return m || "?";
}

// pct renders a 0..1 fraction as a friendly whole/one-decimal percent.
export function pct(frac: number): string {
  const p = (frac || 0) * 100;
  if (p > 0 && p < 1) return p.toFixed(1) + "%";
  return Math.round(p) + "%";
}

// agoFrom is the freshness line on a board: how old its newest reading is, and
// whether that is old enough to call stale (over two minutes — a board polls
// every 12 s, so anything older means the gateway has seen no traffic or the
// panel can't reach it).
export function agoFrom(iso: string): { text: string; stale: boolean } {
  if (!iso || iso.startsWith("0001")) return { text: "no usage recorded yet", stale: false };
  const mins = Math.round((Date.now() - new Date(iso).getTime()) / 60000);
  return { text: "as of " + when(iso), stale: mins > 2 };
}

// Every clock time in the panel is 12-hour with am/pm, whatever locale the
// browser would otherwise pick, so a machine set to a 24-hour locale does not
// read "15:52" here and "3:52 pm" everywhere else on the same screen.
export function clockTime(iso?: string | Date): string {
  if (!iso) return "";
  const d = iso instanceof Date ? iso : new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit", hour12: true }).toLowerCase();
}

// dateAndTime is the same instant with its date, for a tooltip or anything
// that may not be today.
export function dateAndTime(iso?: string | Date): string {
  if (!iso) return "";
  const d = iso instanceof Date ? iso : new Date(iso);
  if (isNaN(d.getTime())) return "";
  return `${d.toLocaleDateString("en-US", { month: "short", day: "numeric" })}, ${clockTime(d)}`;
}

// when renders a past instant in plain words ("3 min ago", "2 days ago"), the
// same phrasing the activity feed and device rows use everywhere.
export function when(iso?: string): string {
  if (!iso || iso.startsWith("0001")) return "";
  const d = new Date(iso);
  const mins = Math.round((Date.now() - d.getTime()) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `${hrs} ${hrs === 1 ? "hour" : "hours"} ago`;
  const days = Math.round(hrs / 24);
  if (days < 7) return `${days} ${days === 1 ? "day" : "days"} ago`;
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

// untilReset says how long until a usage window rolls over ("in 2h 14m"),
// or "resetting…" once the moment has passed but a fresh reading hasn't landed.
export function untilReset(iso?: string): string {
  if (!iso) return "";
  const left = new Date(iso).getTime() - Date.now();
  if (left <= 0) return "resetting…";
  const mins = Math.round(left / 60000);
  if (mins < 60) return `resets in ${mins}m`;
  const h = Math.floor(mins / 60);
  const m = mins % 60;
  if (h < 24) return `resets in ${h}h${m ? " " + m + "m" : ""}`;
  return `resets in ${Math.round(h / 24)}d`;
}

// windowTone maps a window's utilisation to the green/amber/red /usage ramp.
export function windowTone(frac: number): string {
  if (frac >= 0.95) return "#E05C53";
  if (frac >= 0.75) return "#E0A83E";
  return "#46C08A";
}

// untilExpiry says how long an invite has left, for the copy under a link.
export function untilExpiry(iso?: string): string {
  if (!iso) return "for a while";
  const left = new Date(iso).getTime() - Date.now();
  if (left <= 0) return "but it has expired";
  const mins = Math.round(left / 60000);
  if (mins < 60) return `${mins} min left`;
  return `${Math.round(mins / 60)} hr left`;
}

// accessLeft is the short tail on an access chip with a deadline: "6h left",
// "3d left", or "ended" once it has passed (the panel removes it seconds later).
export function accessLeft(iso?: string): string {
  if (!iso || iso.startsWith("0001")) return "";
  const left = new Date(iso).getTime() - Date.now();
  if (left <= 0) return "ended";
  const mins = Math.round(left / 60000);
  if (mins < 60) return `${mins}m left`;
  const hrs = Math.round(mins / 60);
  if (hrs < 48) return `${hrs}h left`;
  return `${Math.round(hrs / 24)}d left`;
}
