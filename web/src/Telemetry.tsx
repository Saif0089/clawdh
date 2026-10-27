import { useEffect, useMemo, useRef, useState } from "react";
import { api, Panel, SeriesPoint, Telemetry as Tel } from "./api";
import { fmtNum } from "./format";
import { Seg } from "./UsageBoard";

// Usage over time for one person, or for everyone: what the gateway metered,
// cut as finely as a second or as coarsely as a month.
//
// Nothing is estimated. A bar is the weighted tokens metered in its bucket and a
// bucket with nothing in it is an empty slot, not a guess. The fine grains are
// cut from individual requests, so they carry a request count; the longer ones
// are summed from the hourly totals the gateway keeps with each request, and
// agree with them to the token.
//
// It stays cheap by asking for less each time. The whole view is read once; after
// that a sync sends the end of what is already held and gets back the newest
// bucket, and anything older than the view is dropped, so a chart left open all
// day holds the same few hundred points it started with.

type Grain = "second" | "minute" | "hour" | "day" | "week" | "month" | "all";

const GRAINS: [Grain, string][] = [
  ["second", "Seconds"], ["minute", "Minutes"], ["hour", "Hours"], ["day", "Days"], ["week", "Weeks"], ["month", "Months"], ["all", "All time"],
];

// How far back each grain reaches, how long a bucket is, and how often it syncs.
// The reach and length mirror panel.Resolutions; months have no fixed length.
const SPAN: Record<Grain, number> = { second: 5 * 60e3, minute: 2 * 3600e3, hour: 72 * 3600e3, day: 30 * 864e5, week: 26 * 7 * 864e5, month: 730 * 864e5, all: 0 };
const STEP: Record<Grain, number> = { second: 1e3, minute: 60e3, hour: 3600e3, day: 864e5, week: 7 * 864e5, month: 0, all: 0 };
const SYNC_MS: Record<Grain, number> = { second: 3000, minute: 10000, hour: 30000, day: 60000, week: 60000, month: 60000, all: 60000 };

// startOf cuts an instant to its bucket in UTC, the way the panel does: weeks
// begin on Monday and a month on the 1st.
function startOf(t: number, g: Grain): number {
  switch (g) {
    case "second": case "minute": case "hour": case "day": return Math.floor(t / STEP[g]) * STEP[g];
    case "week": {
      const day = Math.floor(t / 864e5) * 864e5;
      return day - ((new Date(day).getUTCDay() + 6) % 7) * 864e5;
    }
    default: {
      const d = new Date(t);
      return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
    }
  }
}
function nextBucket(t: number, g: Grain): number {
  if (g === "month" || g === "all") {
    const d = new Date(t);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
  }
  return t + STEP[g];
}

// slots is every bucket the view covers, so a quiet stretch shows as the gap it is.
function slots(points: SeriesPoint[], g: Grain, now: number): number[] {
  const first = g === "all" ? (points.length ? startOf(Date.parse(points[0].t), g) : startOf(now, g)) : startOf(now - SPAN[g], g);
  const out: number[] = [];
  for (let t = first; t <= now; t = nextBucket(t, g)) out.push(t);
  return out;
}

// merge folds a sync into what is held: the newest bucket replaces its older
// self, anything new is added, and anything that has slid out of the view goes.
function merge(held: SeriesPoint[], incoming: SeriesPoint[], g: Grain, now: number): SeriesPoint[] {
  const byTime = new Map<number, SeriesPoint>();
  for (const p of held) byTime.set(Date.parse(p.t), p);
  for (const p of incoming) byTime.set(Date.parse(p.t), p);
  const floor = g === "all" ? -Infinity : startOf(now - SPAN[g], g);
  return [...byTime.entries()].filter(([t]) => t >= floor).sort((a, b) => a[0] - b[0]).map(([, p]) => p);
}

function label(ms: number, g: Grain): string {
  const d = new Date(ms);
  switch (g) {
    case "second": return d.toLocaleTimeString("en-GB");
    case "minute": return d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
    case "hour": return d.toLocaleDateString("en-US", { weekday: "short" }) + " " + d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
    case "day": case "week": return d.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
    default: return d.toLocaleDateString("en-US", { month: "short", year: "numeric", timeZone: "UTC" });
  }
}

// niceMax rounds the tallest bar up to a figure an axis can be ticked in.
function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
}

const BAR = "#7C8797"; // the board's one neutral hue: colour here would mean something it does not

export function TelemetryTab() {
  const [grain, setGrain] = useState<Grain>("minute");
  const [person, setPerson] = useState("");
  const [people, setPeople] = useState<{ id: string; name: string }[]>([]);
  const [points, setPoints] = useState<SeriesPoint[]>([]);
  const [now, setNow] = useState(() => Date.now());
  const [err, setErr] = useState("");
  const [loaded, setLoaded] = useState(false);
  const held = useRef<SeriesPoint[]>([]);

  useEffect(() => {
    api<Panel>("GET", "/api/panel").then((d) => setPeople(d.people.map((p) => ({ id: p.id, name: p.name })))).catch(() => {});
  }, []);

  useEffect(() => {
    let live = true;
    held.current = [];
    setPoints([]);
    setLoaded(false);
    const read = (since?: string) =>
      api<Tel>("GET", `/api/usage/telemetry?res=${grain}&person=${encodeURIComponent(person)}` + (since ? `&since=${encodeURIComponent(since)}` : ""));
    const sync = () => {
      if (document.hidden) return; // a tab nobody is looking at asks for nothing
      const last = held.current.length ? held.current[held.current.length - 1].t : undefined;
      read(last)
        .then((t) => {
          if (!live) return;
          const at = Date.now();
          held.current = merge(held.current, t.points || [], grain, at);
          setPoints(held.current);
          setNow(at);
          setErr("");
          setLoaded(true);
        })
        .catch((e) => live && setErr(e.message)); // keep what is on screen through a hiccup
    };
    sync();
    const id = setInterval(sync, SYNC_MS[grain]);
    const visible = () => !document.hidden && sync();
    document.addEventListener("visibilitychange", visible);
    return () => {
      live = false;
      clearInterval(id);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [grain, person]);

  const who = person === "" ? "Everyone" : people.find((p) => p.id === person)?.name || "This person";
  const total = points.reduce((a, p) => a + p.weighted, 0);
  const tokens = points.reduce((a, p) => a + p.tokens, 0);
  const requests = points.reduce((a, p) => a + (p.requests || 0), 0);
  const peak = points.reduce<SeriesPoint | null>((m, p) => (!m || p.weighted > m.weighted ? p : m), null);
  const raw = grain === "second" || grain === "minute";

  return (
    <div>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-[28px] font-bold tracking-tight">Telemetry</h1>
          <div className="mt-1 text-[15px] text-muted">What the gateway metered, as finely as a second. Nothing here is estimated.</div>
        </div>
        <select
          aria-label="Whose usage"
          value={person}
          onChange={(e) => setPerson(e.target.value)}
          className="rounded-xl border border-line bg-sunken px-3.5 py-2 text-[14.5px] text-ink outline-none focus:border-primary/60"
        >
          <option value="">Everyone</option>
          {people.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
        </select>
      </div>

      <div className="mt-5 overflow-x-auto pb-1">
        <Seg value={grain} onChange={setGrain} options={GRAINS} />
      </div>

      {err && <div className="mt-4 rounded-xl border border-crit/30 bg-crit/5 px-4 py-2.5 text-[14px] text-crit">{err}</div>}

      <div className="mt-5 rounded-2xl border border-line bg-raised p-5">
        <div className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
          <Stat label={`${who}, in view`} value={fmtNum(total)} note="weighted tokens" />
          <Stat label="Raw tokens" value={fmtNum(tokens)} note="every class added up" />
          <Stat label="Busiest" value={peak ? fmtNum(peak.weighted) : "—"} note={peak ? label(Date.parse(peak.t), grain) : ""} />
          <Stat label="Requests" value={raw ? fmtNum(requests) : "—"} note={raw ? "counted per request" : "not counted at this grain"} />
        </div>
        <Chart points={points} grain={grain} now={now} loaded={loaded} />
        <div className="mt-3 text-[12.5px] leading-snug text-faint">
          Seconds and minutes are cut from individual requests. Hours and longer come from the hourly totals the gateway keeps with them.
          {grain === "day" || grain === "week" || grain === "month" || grain === "all" ? " Days, weeks and months are UTC; weeks begin on Monday." : ""}
        </div>
      </div>
    </div>
  );
}

function Stat({ label: l, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="min-w-0">
      <div className="truncate text-[12px] font-medium uppercase tracking-[0.08em] text-faint">{l}</div>
      <div className="mt-0.5 text-[22px] font-semibold tabular-nums text-ink">{value}</div>
      <div className="truncate text-[12px] text-faint">{note}</div>
    </div>
  );
}

function Chart({ points, grain, now, loaded }: { points: SeriesPoint[]; grain: Grain; now: number; loaded: boolean }) {
  const box = useRef<HTMLDivElement>(null);
  const [w, setW] = useState(640);
  const [hover, setHover] = useState<number | null>(null);
  useEffect(() => {
    if (!box.current) return;
    const ro = new ResizeObserver(() => box.current && setW(box.current.clientWidth));
    ro.observe(box.current);
    setW(box.current.clientWidth);
    return () => ro.disconnect();
  }, []);

  const times = useMemo(() => slots(points, grain, now), [points, grain, now]);
  const byTime = useMemo(() => new Map(points.map((p) => [Date.parse(p.t), p])), [points]);
  const vals = times.map((t) => byTime.get(t)?.weighted || 0);
  const max = niceMax(Math.max(0, ...vals));

  const H = 240, padL = 48, padR = 8, padT = 10, padB = 26;
  const slot = (w - padL - padR) / Math.max(times.length, 1);
  const bw = Math.max(1, slot - (slot > 4 ? 1.5 : 0));
  const y = (v: number) => padT + (H - padT - padB) * (1 - v / max);
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * max);
  const every = Math.max(1, Math.ceil(times.length / Math.max(2, Math.floor((w - padL) / 92))));

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const i = Math.floor((e.clientX - r.left - padL) / slot);
    setHover(i >= 0 && i < times.length ? i : null);
  };
  const hp = hover != null ? byTime.get(times[hover]) : undefined;

  return (
    <div ref={box} className="relative mt-5">
      <svg width={w} height={H} viewBox={`0 0 ${w} ${H}`} role="img" aria-label="Weighted tokens metered per bucket" onMouseMove={onMove} onMouseLeave={() => setHover(null)} className="block">
        {ticks.map((v, i) => (
          <g key={i}>
            <line x1={padL} x2={w - padR} y1={y(v)} y2={y(v)} className="stroke-line" strokeWidth={1} opacity={i === 0 ? 1 : 0.5} />
            <text x={padL - 8} y={y(v) + 4} textAnchor="end" className="fill-faint text-[11px] tabular-nums">{v === 0 ? "0" : fmtNum(v)}</text>
          </g>
        ))}
        {times.map((t, i) => {
          const v = vals[i];
          const x = padL + i * slot + (slot - bw) / 2;
          return (
            <g key={t}>
              {v > 0 && <rect x={x} y={y(v)} width={bw} height={Math.max(1, H - padB - y(v))} rx={bw > 5 ? 2 : 0} fill={BAR} opacity={hover === i ? 1 : 0.85} />}
              {i % every === 0 && <text x={x + bw / 2} y={H - 8} textAnchor="middle" className="fill-faint text-[11px] tabular-nums">{label(t, grain)}</text>}
            </g>
          );
        })}
        {hover != null && <rect x={padL + hover * slot} y={padT} width={slot} height={H - padT - padB} className="fill-ink" opacity={0.05} />}
      </svg>
      {hover != null && (
        <div
          className="pointer-events-none absolute z-10 rounded-lg border border-line bg-raised px-3 py-2 text-[12.5px] shadow-lg"
          style={{ left: Math.min(Math.max(padL + hover * slot - 70, 0), Math.max(0, w - 170)), top: 0 }}
        >
          <div className="font-medium text-ink">{label(times[hover], grain)}</div>
          <div className="tabular-nums text-ink">{fmtNum(hp?.weighted || 0)} <span className="text-faint">weighted</span></div>
          <div className="tabular-nums text-muted">{fmtNum(hp?.tokens || 0)} <span className="text-faint">raw tokens</span></div>
          {hp && hp.requests ? <div className="tabular-nums text-muted">{hp.requests} <span className="text-faint">{hp.requests === 1 ? "request" : "requests"}</span></div> : null}
        </div>
      )}
      {loaded && points.length === 0 && (
        <div className="absolute inset-x-0 top-1/3 text-center text-[14px] text-muted">Nothing was metered in this view.</div>
      )}
    </div>
  );
}
