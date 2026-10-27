import { useEffect, useMemo, useRef, useState } from "react";
import { api, Panel, SeriesPoint, SessionUsage, Telemetry as Tel } from "./api";
import { fmtNum } from "./format";

// Usage over time, for everyone or one person, readable at a glance: four
// figures, what the usage went on, one chart, and the person's sessions. The
// only choice is the scope, every session or one of them.
//
// Nothing is estimated. Every figure is what the gateway metered, in the one
// unit the boards use (usage: each token class at its rate, times the model's),
// and a bucket with nothing in it is an empty slot, not a guess. It stays cheap:
// the minutes sync from the end of what is held, and the longer views are a few
// hundred rows refreshed once a minute; a hidden tab asks for nothing.

type Grain = "minute" | "hour" | "day";
type ClassKey = "input" | "output" | "cacheWrite" | "cacheRead";

const STEP: Record<Grain, number> = { minute: 60e3, hour: 3600e3, day: 864e5 };
const GRAIN_WORD: Record<Grain, string> = { minute: "per minute", hour: "per hour", day: "per day" };

// The token classes in plain words, bottom of a stack first. Colour means the
// class and nothing else, and every class is named beside its swatch.
const CLASSES: { key: ClassKey; label: string; color: string }[] = [
  { key: "cacheRead", label: "Re-reading the conversation", color: "#9085e9" },
  { key: "cacheWrite", label: "Adding new context", color: "#199e70" },
  { key: "input", label: "New input", color: "#3987e5" },
  { key: "output", label: "Claude writing", color: "#d95926" },
];

// What the biggest part of the usage means, in a sentence.
const MEANING: Record<ClassKey, string> = {
  cacheRead: "Claude re-reading the conversation on every request. It grows as a conversation gets longer.",
  cacheWrite: "adding new context: files, tool results and messages entering the conversation.",
  input: "new input that was not cached.",
  output: "Claude writing: answers, code and tool calls.",
};

const FOOTNOTE =
  "Exact, from the gateway. Usage is counted the way plans count it: Claude writing weighs 5×, adding context 1.25×, re-reading 0.1× a new input token, times the model's rate.";

type Mix = Record<ClassKey, number>;
const noMix = (): Mix => ({ input: 0, output: 0, cacheWrite: 0, cacheRead: 0 });
function mixOf(points: SeriesPoint[]): Mix {
  const m = noMix();
  for (const p of points) for (const c of CLASSES) m[c.key] += p[c.key]?.weighted || 0;
  return m;
}
const sumOf = (points: SeriesPoint[], f: (p: SeriesPoint) => number) => points.reduce((a, p) => a + f(p), 0);

function startOf(t: number, g: Grain): number {
  return Math.floor(t / STEP[g]) * STEP[g];
}
// merge folds a sync into what is held: the newest bucket replaces its older
// self, and anything older than floor goes.
function merge(held: SeriesPoint[], incoming: SeriesPoint[], floor: number): SeriesPoint[] {
  const by = new Map<number, SeriesPoint>();
  for (const p of held) by.set(Date.parse(p.t), p);
  for (const p of incoming) by.set(Date.parse(p.t), p);
  return [...by.entries()].filter(([t]) => t >= floor).sort((a, b) => a[0] - b[0]).map(([, p]) => p);
}

// A session's chart is cut finely enough to show its shape: minutes for a short
// one, hours for up to a few days, days past that.
function grainFor(spanMs: number): Grain {
  if (spanMs <= 2 * 3600e3) return "minute";
  if (spanMs <= 3 * 864e5) return "hour";
  return "day";
}

function span(ms: number): string {
  const m = Math.max(1, Math.round(ms / 60e3));
  if (m < 60) return `${m} min`;
  const h = Math.floor(m / 60), r = m % 60;
  if (h < 48) return r ? `${h} h ${r} min` : `${h} h`;
  return `${Math.round(h / 24)} days`;
}
function when(ms: number): string {
  const d = new Date(ms);
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" }) + ", " + d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
}
function label(ms: number, g: Grain): string {
  const d = new Date(ms);
  if (g === "minute") return d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  if (g === "hour") return d.toLocaleDateString("en-US", { weekday: "short" }) + " " + d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
}
function niceMax(v: number): number {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  const n = v / p;
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
}
const shortId = (id: string) => "Session " + id.slice(0, 8);

// useVisibleInterval runs fn now and every ms while the tab is shown, and again
// when it comes back into view.
function useVisibleInterval(fn: () => void, ms: number, deps: unknown[]) {
  useEffect(() => {
    const run = () => !document.hidden && fn();
    run();
    const id = setInterval(run, ms);
    document.addEventListener("visibilitychange", run);
    return () => {
      clearInterval(id);
      document.removeEventListener("visibilitychange", run);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
}

export function TelemetryTab() {
  const [person, setPerson] = useState("");
  const [people, setPeople] = useState<{ id: string; name: string }[]>([]);
  const [session, setSession] = useState("");
  const [sessions, setSessions] = useState<SessionUsage[]>([]);
  const [minutes, setMinutes] = useState<SeriesPoint[]>([]);
  const [hours, setHours] = useState<SeriesPoint[]>([]);
  const [days, setDays] = useState<SeriesPoint[]>([]);
  const [own, setOwn] = useState<SeriesPoint[]>([]); // the chosen session's buckets
  const [now, setNow] = useState(() => Date.now());
  const [err, setErr] = useState("");
  const [loaded, setLoaded] = useState(false);
  const heldMinutes = useRef<SeriesPoint[]>([]);

  useEffect(() => {
    api<Panel>("GET", "/api/panel").then((d) => setPeople(d.people.map((p) => ({ id: p.id, name: p.name })))).catch(() => {});
  }, []);

  const q = `&person=${encodeURIComponent(person)}`;
  const read = (res: Grain, extra = "") => api<Tel>("GET", `/api/usage/telemetry?res=${res}${q}${extra}`);
  const failed = (e: Error) => setErr(e.message); // keep what is on screen through a hiccup

  // A new person starts from nothing.
  useEffect(() => {
    heldMinutes.current = [];
    setMinutes([]); setHours([]); setDays([]); setSessions([]); setSession(""); setLoaded(false);
  }, [person]);

  // Every session: the last two hours by the minute, synced from what is held.
  useVisibleInterval(() => {
    const held = heldMinutes.current;
    const since = held.length ? `&since=${encodeURIComponent(held[held.length - 1].t)}` : "";
    read("minute", since)
      .then((t) => {
        const at = Date.now();
        heldMinutes.current = merge(heldMinutes.current, t.points || [], startOf(at - 2 * 3600e3, "minute"));
        setMinutes(heldMinutes.current);
        setNow(at);
        setErr("");
        setLoaded(true);
      })
      .catch(failed);
  }, 10000, [person]);

  // The day and the month, and the person's sessions, once a minute.
  useVisibleInterval(() => {
    read("hour").then((t) => setHours(t.points || [])).catch(failed);
    read("day").then((t) => setDays(t.points || [])).catch(failed);
    if (person) api<{ sessions: SessionUsage[] }>("GET", `/api/usage/telemetry/sessions?person=${encodeURIComponent(person)}`).then((b) => setSessions(b.sessions || [])).catch(failed);
  }, 60000, [person]);

  const chosen = sessions.find((s) => s.id === session);
  const sFirst = chosen ? Date.parse(chosen.first) : 0;
  const sLast = chosen ? Date.parse(chosen.last) : 0;
  const sGrain = grainFor(sLast - sFirst);

  // One session: its whole span, at a grain its length calls for.
  useVisibleInterval(() => {
    if (!session || !chosen) return;
    read(sGrain, `&session=${encodeURIComponent(session)}&since=${encodeURIComponent(new Date(startOf(sFirst, sGrain)).toISOString().replace(/\.\d+Z$/, "Z"))}`)
      .then((t) => { setOwn(t.points || []); setErr(""); })
      .catch(failed);
  }, 15000, [person, session, sGrain, sFirst]);
  useEffect(() => setOwn([]), [session]);

  const who = person === "" ? "Everyone" : people.find((p) => p.id === person)?.name || "This person";

  // The figures for the scope.
  const hourAgo = startOf(now - 3600e3, "minute");
  const lastHour = minutes.filter((p) => Date.parse(p.t) >= hourAgo);
  const dayAgo = startOf(now - 24 * 3600e3, "hour");
  const lastDay = hours.filter((p) => Date.parse(p.t) >= dayAgo);
  const weekAgo = startOf(now, "day") - 6 * 864e5;
  const tiles: { label: string; value: string; note?: string }[] = chosen
    ? [
        { label: "Total", value: fmtNum(chosen.usage.weighted), note: "usage" },
        { label: "Requests", value: fmtNum(chosen.usage.requests || 0) },
        { label: "Per request", value: chosen.usage.requests ? fmtNum(chosen.usage.weighted / chosen.usage.requests) : "—", note: "on average" },
        { label: "Active", value: span(sLast - sFirst), note: "from " + when(sFirst) },
      ]
    : [
        { label: "Last hour", value: fmtNum(sumOf(lastHour, (p) => p.weighted)), note: `${fmtNum(sumOf(lastHour, (p) => p.requests || 0))} requests` },
        { label: "Last 24 hours", value: fmtNum(sumOf(lastDay, (p) => p.weighted)) },
        { label: "Last 7 days", value: fmtNum(sumOf(days.filter((p) => Date.parse(p.t) >= weekAgo), (p) => p.weighted)) },
        { label: "Last 30 days", value: fmtNum(sumOf(days, (p) => p.weighted)) },
      ];
  const mix = chosen ? mixOf([chosen.usage]) : mixOf(lastDay);
  const chart = chosen
    ? { points: own, grain: sGrain, from: startOf(sFirst, sGrain), to: Math.max(sLast, sFirst), title: `This session · ${GRAIN_WORD[sGrain]}` }
    : { points: minutes, grain: "minute" as Grain, from: startOf(now - 2 * 3600e3, "minute") + 60e3, to: now, title: "Last 2 hours · per minute" };

  return (
    <div>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-[28px] font-bold tracking-tight">Telemetry</h1>
          <div className="mt-1 text-[15px] text-muted">{who}: what the gateway metered.</div>
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

      {chosen && (
        <div className="mt-5 flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-primary/50 bg-raised-2 px-4 py-2.5 text-[14px]">
          <span className="text-muted">Showing</span>
          <span className="font-semibold text-ink">{shortId(chosen.id)}</span>
          <button type="button" onClick={() => setSession("")} className="ml-auto rounded-lg px-2.5 py-1 text-[13.5px] font-medium text-primary hover:bg-raised">
            Show all sessions
          </button>
        </div>
      )}

      {err && <div className="mt-4 rounded-xl border border-crit/30 bg-crit/5 px-4 py-2.5 text-[14px] text-crit">{err}</div>}

      <div className="mt-5 grid grid-cols-2 gap-3 sm:grid-cols-4">
        {tiles.map((t) => (
          <div key={t.label} className="min-w-0 rounded-2xl border border-line bg-raised px-4 py-3.5">
            <div className="truncate text-[12.5px] font-medium text-muted">{t.label}</div>
            <div className="mt-1 text-[26px] font-semibold leading-tight tabular-nums text-ink">{t.value}</div>
            {t.note && <div className="truncate text-[12.5px] text-faint">{t.note}</div>}
          </div>
        ))}
      </div>

      <SpentOn mix={mix} period={chosen ? "this session" : "the last 24 hours"} />

      <div className="mt-4 rounded-2xl border border-line bg-raised p-5">
        <div className="text-[15px] font-semibold text-ink">{chart.title}</div>
        <Chart {...chart} loaded={loaded} />
      </div>

      {person && <Sessions list={sessions} chosen={session} onPick={setSession} />}

      <div className="mt-4 text-[12.5px] leading-snug text-faint">{FOOTNOTE}</div>
    </div>
  );
}

// SpentOn is what the usage went on: one bar split by class, each part named,
// and a sentence for the biggest of them.
function SpentOn({ mix, period }: { mix: Mix; period: string }) {
  const total = CLASSES.reduce((a, c) => a + mix[c.key], 0);
  const top = CLASSES.reduce((m, c) => (mix[c.key] > mix[m.key] ? c : m), CLASSES[0]);
  const pct = (v: number) => (total > 0 ? Math.round((v / total) * 100) : 0);
  const share = pct(mix[top.key]);
  return (
    <div className="mt-4 rounded-2xl border border-line bg-raised p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="text-[15px] font-semibold text-ink">What it's spent on</div>
        <div className="text-[12.5px] text-faint">{period}</div>
      </div>
      <div className="mt-3 flex h-2.5 gap-[2px] overflow-hidden rounded-full bg-sunken" role="img" aria-label={CLASSES.map((c) => `${c.label} ${pct(mix[c.key])}%`).join(", ")}>
        {total > 0 && CLASSES.map((c) => mix[c.key] > 0 && <span key={c.key} className="h-full first:rounded-l-full last:rounded-r-full" style={{ width: `${(mix[c.key] / total) * 100}%`, background: c.color }} />)}
      </div>
      <div className="mt-3 flex flex-wrap gap-x-6 gap-y-2 text-[13.5px]">
        {CLASSES.map((c) => (
          <div key={c.key} className="flex items-center gap-2">
            <span className="h-2.5 w-2.5 shrink-0 rounded-sm" style={{ background: c.color }} />
            <span className="text-muted">{c.label}</span>
            <span className="font-semibold tabular-nums text-ink">{pct(mix[c.key])}%</span>
            <span className="tabular-nums text-faint">{fmtNum(mix[c.key])}</span>
          </div>
        ))}
      </div>
      {total > 0 && (
        <div className="mt-3 text-[14px] text-ink">
          {share >= 50 ? "Most of it" : "The biggest part"} ({share}%) is {MEANING[top.key]}
        </div>
      )}
    </div>
  );
}

function Sessions({ list, chosen, onPick }: { list: SessionUsage[]; chosen: string; onPick: (id: string) => void }) {
  const [all, setAll] = useState(false);
  const shown = all ? list : list.slice(0, 6);
  return (
    <div className="mt-4 rounded-2xl border border-line bg-raised p-5">
      <div className="text-[15px] font-semibold text-ink">Sessions</div>
      <div className="mt-0.5 text-[12.5px] text-faint">The last 30 days, newest first, subagents included. Pick one to see it alone.</div>
      {list.length === 0 ? (
        <div className="mt-3 text-[14px] text-muted">No session has been metered yet.</div>
      ) : (
        <div className="mt-3 flex flex-col gap-1.5">
          {shown.map((s) => {
            const on = s.id === chosen;
            const first = Date.parse(s.first), last = Date.parse(s.last);
            const m = mixOf([s.usage]);
            const total = s.usage.weighted;
            return (
              <button
                key={s.id}
                type="button"
                aria-pressed={on}
                title={s.id}
                onClick={() => onPick(on ? "" : s.id)}
                className={`grid w-full grid-cols-1 items-center gap-2 rounded-xl border px-3.5 py-2.5 text-left sm:grid-cols-[minmax(0,1.5fr)_minmax(80px,1fr)_minmax(0,1fr)] sm:gap-4 ${on ? "border-primary/60 bg-raised-2" : "border-line hover:bg-raised-2"}`}
              >
                <div className="min-w-0">
                  <div className="truncate text-[14px] font-semibold text-ink">{shortId(s.id)}</div>
                  <div className="truncate text-[12.5px] text-faint">started {when(first)} · {span(last - first)}</div>
                </div>
                <div className="flex h-2 gap-[2px] overflow-hidden rounded-full bg-sunken">
                  {CLASSES.map((c) => m[c.key] > 0 && total > 0 && <span key={c.key} className="h-full" style={{ width: `${(m[c.key] / total) * 100}%`, background: c.color }} />)}
                </div>
                <div className="min-w-0 sm:text-right">
                  <div className="text-[14px] font-semibold tabular-nums text-ink">{fmtNum(total)}</div>
                  <div className="text-[12.5px] tabular-nums text-faint">≈{fmtNum(s.usage.requests ? total / s.usage.requests : 0)} per request</div>
                </div>
              </button>
            );
          })}
          {list.length > 6 && (
            <button type="button" onClick={() => setAll(!all)} className="mt-1 self-start rounded-lg px-2.5 py-1 text-[13.5px] font-medium text-primary hover:bg-raised-2">
              {all ? "Show fewer" : `Show all ${list.length}`}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

function Chart({ points, grain, from, to, loaded }: { points: SeriesPoint[]; grain: Grain; from: number; to: number; loaded: boolean; title?: string }) {
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

  // slots is every bucket the chart covers, so a quiet stretch shows as the gap it is.
  const times = useMemo(() => {
    const out: number[] = [];
    for (let t = startOf(from, grain); t <= to; t += STEP[grain]) out.push(t);
    return out;
  }, [from, to, grain]);
  const byTime = useMemo(() => new Map(points.map((p) => [Date.parse(p.t), p])), [points]);
  const vals = times.map((t) => byTime.get(t)?.weighted || 0);
  const max = niceMax(Math.max(0, ...vals));

  const H = 220, padL = 48, padR = 8, padT = 10, padB = 26;
  const slot = (w - padL - padR) / Math.max(times.length, 1);
  const bw = Math.max(1, slot - (slot > 4 ? 1.5 : 0));
  const y = (v: number) => padT + (H - padT - padB) * (1 - v / max);
  const every = Math.max(1, Math.ceil(times.length / Math.max(2, Math.floor((w - padL) / 92))));

  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const i = Math.floor((e.clientX - r.left - padL) / slot);
    setHover(i >= 0 && i < times.length ? i : null);
  };
  const hp = hover != null ? byTime.get(times[hover]) : undefined;

  return (
    <div ref={box} className="relative mt-3">
      <svg width={w} height={H} viewBox={`0 0 ${w} ${H}`} role="img" aria-label="Usage per bucket, by what it went on" onMouseMove={onMove} onMouseLeave={() => setHover(null)} className="block">
        {[0, 0.5, 1].map((f, i) => (
          <g key={i}>
            <line x1={padL} x2={w - padR} y1={y(f * max)} y2={y(f * max)} className="stroke-line" strokeWidth={1} opacity={i === 0 ? 1 : 0.5} />
            <text x={padL - 8} y={y(f * max) + 4} textAnchor="end" className="fill-faint text-[11px] tabular-nums">{f === 0 ? "0" : fmtNum(f * max)}</text>
          </g>
        ))}
        {times.map((t, i) => {
          const x = padL + i * slot + (slot - bw) / 2;
          const p = byTime.get(t);
          let acc = 0;
          const segs = p ? CLASSES.map((c) => ({ v: p[c.key]?.weighted || 0, color: c.color })).filter((s) => s.v > 0) : [];
          return (
            <g key={t} opacity={hover == null || hover === i ? 1 : 0.8}>
              {segs.map((sg, j) => {
                const top = y(acc + sg.v), bottom = y(acc);
                acc += sg.v;
                const gap = j > 0 && bottom - top > 3 ? 1 : 0; // a surface gap between parts, where there is room
                return <rect key={j} x={x} y={top} width={bw} height={Math.max(j === 0 ? 1 : 0, bottom - top - gap)} rx={bw > 5 && j === segs.length - 1 ? 2 : 0} fill={sg.color} />;
              })}
              {i % every === 0 && <text x={x + bw / 2} y={H - 8} textAnchor="middle" className="fill-faint text-[11px] tabular-nums">{label(t, grain)}</text>}
            </g>
          );
        })}
        {hover != null && <rect x={padL + hover * slot} y={padT} width={slot} height={H - padT - padB} className="fill-ink" opacity={0.05} />}
      </svg>
      {hover != null && (
        <div
          className="pointer-events-none absolute z-10 min-w-[310px] rounded-lg border border-line bg-raised px-3 py-2 text-[12.5px] shadow-lg"
          style={{ left: Math.min(Math.max(padL + hover * slot - 155, 0), Math.max(0, w - 320)), top: 0 }}
        >
          <div className="font-medium text-ink">{label(times[hover], grain)}</div>
          {CLASSES.slice().reverse().map((c) => (
            <div key={c.key} className="flex items-center gap-2 tabular-nums">
              <span className="h-2 w-2 shrink-0 rounded-sm" style={{ background: c.color }} />
              <span className="whitespace-nowrap text-muted">{c.label}</span>
              <span className="ml-auto pl-3 text-ink">{fmtNum(hp?.[c.key]?.weighted || 0)}</span>
              <span className="w-[72px] text-right text-faint">{fmtNum(hp?.[c.key]?.tokens || 0)} tok</span>
            </div>
          ))}
          <div className="mt-1 flex gap-2 border-t border-line pt-1 tabular-nums">
            <span className="text-muted">Total</span>
            <span className="ml-auto font-semibold text-ink">{fmtNum(hp?.weighted || 0)}</span>
            <span className="w-[72px] text-right text-faint">{fmtNum(hp?.tokens || 0)} tok</span>
          </div>
          {hp?.requests ? <div className="tabular-nums text-muted">{hp.requests} {hp.requests === 1 ? "request" : "requests"}</div> : null}
        </div>
      )}
      {loaded && points.length === 0 && <div className="absolute inset-x-0 top-1/3 text-center text-[14px] text-muted">Nothing was metered in this view.</div>}
    </div>
  );
}
