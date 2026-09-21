import { useEffect, useRef, useState } from "react";
import { motion } from "framer-motion";
import { api, Board, Subject, AccountWindow, ModelUsage } from "./api";
import { fmtNum, personColor, modelColor, agoFrom, untilReset, windowTone, pct, when } from "./format";

// The board shows the two things the panel actually knows, and nothing else.
//
// Accounts: each shared login's rolling 5-hour and weekly windows — Claude's
// own numbers for how full they are, read by the gateway — and who filled the
// weekly one. People: who ran what through the gateway over a rolling day,
// week or month, ranked, as a share of the team, split by model.
//
// Every figure is per account and per person as the gateway attributes it, so
// an editor session and a terminal session land on the same rows.

type Window = "day" | "week" | "month";

const WINDOW_LABEL: Record<Window, string> = { day: "Last 24 hours", week: "Last 7 days", month: "Last 30 days" };

// A model's share of a total: label, colour and fraction, for a split bar.
interface Split { label: string; color: string; value: number; frac: number }

function modelSplit(models: ModelUsage[]): Split[] {
  const byLabel = new Map<string, Split>();
  for (const m of models) {
    if (m.weighted <= 0) continue;
    const { label, color } = modelColor(m.model);
    const s = byLabel.get(label) || { label, color, value: 0, frac: 0 };
    s.value += m.weighted;
    byLabel.set(label, s);
  }
  const out = [...byLabel.values()].sort((a, b) => b.value - a.value);
  const total = out.reduce((a, s) => a + s.value, 0);
  for (const s of out) s.frac = total > 0 ? s.value / total : 0;
  return out;
}

// --- controls --------------------------------------------------------------

function Seg<T extends string>({ value: v, onChange, options }: { value: T; onChange: (v: T) => void; options: [T, string][] }) {
  return (
    <div className="inline-flex rounded-xl border border-line bg-sunken p-1">
      {options.map(([opt, label]) => (
        <button key={opt} onClick={() => onChange(opt)} className="relative rounded-lg px-3.5 py-1.5 text-[14px] font-medium transition-colors">
          {opt === v && (
            <motion.span layoutId="seg-active" className="absolute inset-0 rounded-lg bg-raised-2" transition={{ type: "spring", stiffness: 500, damping: 38 }} />
          )}
          <span className={`relative ${opt === v ? "text-ink" : "text-muted hover:text-ink"}`}>{label}</span>
        </button>
      ))}
    </div>
  );
}

// --- accounts --------------------------------------------------------------

function WindowRow({ label, frac, reset }: { label: string; frac: number; reset?: string }) {
  const color = windowTone(frac);
  return (
    <div>
      <div className="flex items-baseline justify-between text-[13.5px]">
        <span className="text-muted">{label}</span>
        <span className="font-semibold tabular-nums" style={{ color }}>{pct(frac)}</span>
      </div>
      <div className="mt-1.5 h-2.5 overflow-hidden rounded-full bg-sunken">
        <motion.div className="h-full rounded-full" style={{ background: color }} initial={{ width: 0 }} animate={{ width: `${Math.min(frac, 1) * 100}%` }} transition={{ duration: 0.6, ease: [0.2, 0.8, 0.2, 1] }} />
      </div>
      {reset && <div className="mt-1 text-[12px] text-faint">{untilReset(reset)}</div>}
    </div>
  );
}

// RanBy names who filled an account's weekly window: one line per person with
// their share of what the gateway metered on it, largest first. The bar is the
// split; the window's own % (above it) is Claude's number for how full it is.
function RanBy({ people, color }: { people: AccountWindow["ranBy"]; color: (id: string) => string }) {
  const total = people.reduce((a, p) => a + p.weighted, 0);
  if (!people.length || total <= 0) {
    return <div className="mt-4 text-[13px] text-faint">Nobody has run it through the gateway this window.</div>;
  }
  return (
    <div className="mt-4">
      <div className="text-[12px] font-medium uppercase tracking-[0.08em] text-faint">Who ran it this week</div>
      <div className="mt-2 flex h-2 overflow-hidden rounded-full bg-sunken">
        {people.map((p) => (
          <span key={p.id} className="h-full" style={{ width: `${(p.weighted / total) * 100}%`, background: color(p.id) }} title={`${p.name} · ${Math.round((p.weighted / total) * 100)}%`} />
        ))}
      </div>
      <div className="mt-2 flex flex-col gap-1">
        {people.map((p) => (
          <div key={p.id} className="flex items-center gap-2 text-[13.5px]">
            <span className="h-2 w-2 shrink-0 rounded-full" style={{ background: color(p.id) }} />
            <span className="min-w-0 flex-1 truncate text-ink">{p.name}</span>
            <span className="tabular-nums text-muted">{Math.round((p.weighted / total) * 100)}%</span>
          </div>
        ))}
      </div>
    </div>
  );
}

function AccountCard({ a, color }: { a: AccountWindow; color: (id: string) => string }) {
  return (
    <motion.div whileHover={{ y: -2 }} transition={{ duration: 0.15 }} className="rounded-2xl border border-line bg-raised p-5 hover:ring-1 hover:ring-primary/20">
      <div className="flex items-baseline justify-between gap-3">
        <div className="min-w-0 truncate text-[15px] font-semibold">{a.name}</div>
        {a.hasReading && <div className="shrink-0 text-[12px] text-faint">read {when(a.updatedAt)}</div>}
      </div>
      {a.hasReading ? (
        <div className="mt-3.5 flex flex-col gap-3.5">
          <WindowRow label="5-hour window" frac={a.fiveH} reset={a.fiveHReset} />
          <WindowRow label="Weekly window" frac={a.sevenD} reset={a.sevenDReset} />
        </div>
      ) : (
        <div className="mt-3 text-[13.5px] leading-relaxed text-muted">No reading yet — the gateway reads each login's windows every few minutes.</div>
      )}
      <RanBy people={a.ranBy || []} color={color} />
    </motion.div>
  );
}

// --- people ----------------------------------------------------------------

function PersonRow({ p, rank, teamTotal, color }: { p: Subject; rank: number; teamTotal: number; color: string }) {
  const share = teamTotal > 0 ? p.weighted / teamTotal : 0;
  const split = modelSplit(p.byModel || []);
  return (
    <div className="grid grid-cols-[28px_minmax(0,1fr)_96px] items-center gap-x-4 gap-y-1 py-3 sm:grid-cols-[28px_180px_minmax(0,1fr)_96px]">
      <div className="text-[14px] tabular-nums text-faint">{rank}</div>
      <div className="flex min-w-0 items-center gap-2">
        <span className="h-2.5 w-2.5 shrink-0 rounded-full" style={{ background: color }} />
        <span className="truncate text-[15px] font-medium text-ink">{p.name}</span>
      </div>
      <div className="col-span-3 sm:col-span-1">
        <div className="flex h-7 overflow-hidden rounded-lg bg-sunken ring-1 ring-inset ring-line/60">
          <motion.div className="flex h-full" initial={{ width: 0 }} animate={{ width: `${share * 100}%` }} transition={{ duration: 0.55, ease: [0.2, 0.8, 0.2, 1] }}>
            {split.map((s) => (
              <span key={s.label} className="h-full border-r border-ground/40 last:border-0" style={{ width: `${s.frac * 100}%`, background: s.color }} title={`${s.label} · ${fmtNum(s.value)} (${Math.round(s.frac * 100)}%)`} />
            ))}
          </motion.div>
        </div>
        <div className="mt-1 flex flex-wrap gap-x-3 text-[12px] text-faint">
          {split.map((s) => (
            <span key={s.label} className="inline-flex items-center gap-1">
              <span className="h-1.5 w-1.5 rounded-full" style={{ background: s.color }} />
              {s.label} {Math.round(s.frac * 100)}%
            </span>
          ))}
        </div>
      </div>
      <div className="col-start-3 row-start-1 text-right sm:col-start-4">
        <div className="text-[15px] font-semibold tabular-nums text-ink">{Math.round(share * 100)}%</div>
        <div className="text-[12px] tabular-nums text-faint">{fmtNum(p.weighted)}</div>
      </div>
    </div>
  );
}

// --- board -----------------------------------------------------------------

const REFRESH_MS = 12000;

export function UsageBoard() {
  const [win, setWin] = useState<Window>("week");
  const [people, setPeople] = useState<Subject[]>([]);
  const [windows, setWindows] = useState<AccountWindow[]>([]);
  const [asOf, setAsOf] = useState("");
  const [err, setErr] = useState("");
  const winRef = useRef(win);
  winRef.current = win;

  useEffect(() => {
    let live = true;
    const load = () => {
      Promise.all([
        api<Board>("GET", `/api/usage/people?window=${winRef.current}`),
        api<{ windows: AccountWindow[]; asOf: string }>("GET", `/api/usage/windows`),
      ])
        .then(([p, w]) => {
          if (!live) return;
          setErr("");
          setPeople(p.subjects || []);
          setWindows(w.windows || []);
          setAsOf(p.asOf || "");
        })
        .catch((e) => live && setErr(e.message)); // keep last-known data on a transient failure
    };
    load();
    const id = setInterval(load, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(id);
    };
  }, [win]);

  // One colour per person, the same on every card and row of the board.
  const ids = new Set<string>();
  for (const p of people) ids.add(p.id);
  for (const w of windows) for (const p of w.ranBy || []) ids.add(p.id);
  const idx = new Map([...ids].sort().map((id, i) => [id, i] as const));
  const color = (id: string) => personColor(idx.get(id) ?? 0);

  const teamTotal = people.reduce((a, p) => a + p.weighted, 0);
  const ranked = [...people].sort((a, b) => b.weighted - a.weighted);
  const fresh = agoFrom(asOf);

  return (
    <div>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-[28px] font-bold tracking-tight">Usage</h1>
          <div className="mt-1 flex items-center gap-2 text-[15px] text-muted">
            <span className={`inline-block h-2 w-2 rounded-full ${fresh.stale ? "bg-warn" : "bg-ok"}`} />
            {fresh.stale ? fresh.text : "live · " + fresh.text}
          </div>
        </div>
      </div>

      {err && <div className="mt-4 rounded-xl border border-crit/30 bg-crit/5 px-4 py-2.5 text-[14px] text-crit">{err}</div>}

      {/* Accounts */}
      <h2 className="mb-1 mt-8 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">Accounts</h2>
      <div className="text-[13px] text-faint">How full each account's rolling windows are — Claude's own numbers — and who filled the week.</div>
      {windows.length === 0 ? (
        <div className="mt-3 rounded-2xl border border-dashed border-line px-6 py-8 text-center text-[15px] text-muted">
          No account with a login yet. Add one from a machine's clawdh page and it shows here.
        </div>
      ) : (
        <div className="mt-3 grid gap-3 sm:grid-cols-2">
          {windows.map((a) => <AccountCard key={a.accountId} a={a} color={color} />)}
        </div>
      )}

      {/* People */}
      <div className="mt-10 flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="mb-1 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">People</h2>
          <div className="text-[13px] text-faint">Who ran what through the gateway, as a share of the team. Each bar is split by model.</div>
        </div>
        <Seg value={win} onChange={setWin} options={[["day", "Day"], ["week", "Week"], ["month", "Month"]]} />
      </div>
      <div className="mt-3 rounded-2xl border border-line bg-raised px-5 py-2">
        <div className="flex items-baseline justify-between border-b border-line py-2.5 text-[13px] text-faint">
          <span>{WINDOW_LABEL[win]} · {ranked.length} {ranked.length === 1 ? "person" : "people"}</span>
          <span className="tabular-nums">team {fmtNum(teamTotal)}</span>
        </div>
        {ranked.length === 0 ? (
          <div className="px-1 py-8 text-center">
            <div className="text-[16px] font-medium text-ink">Nothing in this window yet</div>
            <div className="mx-auto mt-1.5 max-w-md text-[14px] leading-relaxed text-muted">
              It fills in as people run shared accounts through the gateway, from a terminal or an editor alike.
            </div>
          </div>
        ) : (
          <div className="divide-y divide-line">
            {ranked.map((p, i) => <PersonRow key={p.id} p={p} rank={i + 1} teamTotal={teamTotal} color={color(p.id)} />)}
          </div>
        )}
      </div>
      <div className="mt-2 text-[12px] leading-relaxed text-faint">
        Figures are tokens weighted to Sonnet-input equivalents, so a person heavy on Opus and one heavy on Haiku compare fairly. They are shares of the team's metered usage, not of an account's window — that is on the cards above.
      </div>
    </div>
  );
}
