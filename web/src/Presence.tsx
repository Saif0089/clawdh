import { Presence } from "./api";

// PresenceBadge says whether a person is working, online or offline right now,
// from what the panel observes: a request the gateway metered in the last three
// minutes is working; a machine of theirs that checked in within the last 90
// seconds (machines check in every 30) is online; neither is offline.
//
// The words carry the meaning; the dot only echoes them. It is outlined and
// neutral on purpose — colour on the usage board means how full a window is —
// and the one filled dot, for working, is a cyan-teal, well apart from the board's green.
const WORKING = "#3AAFC4";

const LABEL: Record<string, string> = { working: "Working", idle: "Online · idle", offline: "Offline" };

// ago is how long since a moment, to the second while it is recent.
export function ago(iso?: string): string {
  if (!iso || iso.startsWith("0001")) return "";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} h ago`;
  return `${Math.round(h / 24)} days ago`;
}

export function presenceTitle(p: Presence): string {
  const t = ago(p.at);
  if (p.state === "working") return `Last request through the gateway ${t}`;
  if (!t) return "No machine of theirs has checked in yet";
  return `Last seen ${t}` + (p.state === "offline" ? " — no machine of theirs has checked in for over 90 seconds" : "");
}

export function PresenceBadge({ p }: { p?: Presence }) {
  if (!p) return null;
  const state = LABEL[p.state] ? p.state : "offline";
  return (
    <span
      title={presenceTitle(p)}
      className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border border-line px-2 py-0.5 text-[11.5px] font-medium leading-none ${state === "offline" ? "text-faint" : "text-muted"}`}
    >
      <span
        aria-hidden
        className={`h-1.5 w-1.5 rounded-full ${state === "idle" ? "border border-muted" : state === "offline" ? "border border-faint/70" : ""}`}
        style={state === "working" ? { background: WORKING } : undefined}
      />
      {LABEL[state]}
    </span>
  );
}
