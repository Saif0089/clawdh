import { useEffect, useRef, useState } from "react";
import { motion } from "framer-motion";
import { api, Board, Subject, AccountUsage, ModelUsage } from "./api";
import { fmtNum, modelLabel, agoFrom, untilReset, windowTone, pct, when } from "./format";

// The board's unit is the account. Each shared login gets one card holding
// everything about it: its rolling 5-hour and weekly windows — Claude's own
// numbers for how full they are — and, for the chosen period, who ran it and
// on which models. A short across-accounts list of people follows.
//
// Colour means one thing here: how full a window is (green, amber, red).
// People and models are never colour-coded — they are labelled rows with one
// neutral bar — so a green bar can only ever mean "plenty of room".
//
// Every figure is per account and per person as the gateway attributes it, so
// an editor session and a terminal session land on the same rows.

type Period = "day" | "week" | "month";

const PERIOD_LABEL: Record<Period, string> = { day: "last 24 hours", week: "last 7 days", month: "last 30 days" };

// The one hue every share bar wears. Neutral on purpose: see above.
const SHARE = "#7C8797";

// modelSplit is a model breakdown in words — "Opus 67% · Sonnet 33%" — folding
// dated model ids into their family.
function modelSplit(models: ModelUsage[]): string {
  const byLabel = new Map<string, number>();
  for (const m of models || []) if (m.weighted > 0) byLabel.set(modelLabel(m.model), (byLabel.get(modelLabel(m.model)) || 0) + m.weighted);
  const total = [...byLabel.values()].reduce((a, v) => a + v, 0);
  if (total <= 0) return "";
  return [...byLabel.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([label, v]) => `${label} ${pct(v / total)}`)
    .join(" · ");
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

// --- pieces ----------------------------------------------------------------

// WindowMeter is one of an account's rolling windows: the only coloured thing
// on the board. The number is always beside the bar, so the state is never
// carried by colour alone.
function WindowMeter({ label, frac, reset }: { label: string; frac: number; reset?: string }) {
  const color = windowTone(frac);
  return (
    <div className="min-w-0 flex-1">
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-[13px] text-muted">{label}</span>
        <span className="text-[12px] text-faint">{untilReset(reset)}</span>
      </div>
      <div className="mt-1 flex items-center gap-3">
        <div className="h-2.5 min-w-0 flex-1 overflow-hidden rounded-full bg-sunken">
          <motion.div className="h-full rounded-full" style={{ background: color }} initial={{ width: 0 }} animate={{ width: `${Math.min(frac, 1) * 100}%` }} transition={{ duration: 0.6, ease: [0.2, 0.8, 0.2, 1] }} />
        </div>
        <span className="w-12 shrink-0 text-right text-[17px] font-semibold tabular-nums text-ink">{pct(frac)}</span>
      </div>
    </div>
  );
}

// TokenRow is one person across every account: a token count with a bar
// relative to the busiest person. Deliberately no percentage — these accounts
// have separate plans and separate weeks, so there is nothing a percentage
// here could honestly be a percentage of.
function TokenRow({ name, detail, value, most }: { name: string; detail: string; value: number; most: number }) {
  const rel = most > 0 ? value / most : 0;
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_96px] items-center gap-x-4 py-2 sm:grid-cols-[minmax(0,200px)_minmax(0,1fr)_96px]">
      <div className="col-start-1 row-start-1 min-w-0">
        <div className="truncate text-[14.5px] font-medium text-ink">{name}</div>
        {detail && <div className="truncate text-[12px] text-faint">{detail}</div>}
      </div>
      <div className="col-span-2 col-start-1 row-start-2 mt-1.5 h-2 overflow-hidden rounded-full bg-sunken sm:col-span-1 sm:col-start-2 sm:row-start-1 sm:mt-0">
        <motion.div className="h-full rounded-full" style={{ background: SHARE }} initial={{ width: 0 }} animate={{ width: `${rel * 100}%` }} transition={{ duration: 0.5, ease: [0.2, 0.8, 0.2, 1] }} />
      </div>
      <div className="col-start-2 row-start-1 text-right text-[14.5px] font-semibold tabular-nums text-ink sm:col-start-3">{fmtNum(value)}</div>
    </div>
  );
}

// ShareRow is one person on an account: how much of the plan's week they
// account for. The bar is drawn against the whole week, not against the other
// people, so the rows stack up to the weekly meter above them and a short bar
// means "barely touched it" rather than "less than the others".
function ShareRow({ name, detail, ofWeekly, value }: { name: string; detail: string; ofWeekly: number; value: number }) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)_96px] items-center gap-x-4 py-2 sm:grid-cols-[minmax(0,200px)_minmax(0,1fr)_96px]">
      <div className="col-start-1 row-start-1 min-w-0">
        <div className="truncate text-[14.5px] font-medium text-ink">{name}</div>
        {detail && <div className="truncate text-[12px] text-faint">{detail}</div>}
      </div>
      <div className="col-span-2 col-start-1 row-start-2 mt-1.5 h-2 overflow-hidden rounded-full bg-sunken sm:col-span-1 sm:col-start-2 sm:row-start-1 sm:mt-0"
           title={`${name} · ${pct(ofWeekly)} of this week · ${fmtNum(value)} tokens`}>
        <motion.div className="h-full rounded-full" style={{ background: SHARE }} initial={{ width: 0 }} animate={{ width: `${Math.min(ofWeekly, 1) * 100}%` }} transition={{ duration: 0.5, ease: [0.2, 0.8, 0.2, 1] }} />
      </div>
      <div className="col-start-2 row-start-1 text-right sm:col-start-3">
        <span className="text-[14.5px] font-semibold tabular-nums text-ink">{pct(ofWeekly)}</span>
        <span className="ml-2 text-[12px] tabular-nums text-faint">{fmtNum(value)}</span>
      </div>
    </div>
  );
}

function AccountCard({ a, period }: { a: AccountUsage; period: Period }) {
  const people = a.people || [];
  return (
    <div className="rounded-2xl border border-line bg-raised p-5">
      <div className="flex items-baseline justify-between gap-3">
        <div className="min-w-0 truncate text-[17px] font-semibold">{a.name}</div>
        <div className="shrink-0 text-[12px] text-faint">{a.hasReading ? "windows read " + when(a.updatedAt) : ""}</div>
      </div>

      {a.hasReading ? (
        <div className="mt-3.5 flex flex-col gap-4 sm:flex-row sm:gap-8">
          <WindowMeter label="5-hour window" frac={a.fiveH} reset={a.fiveHReset} />
          <WindowMeter label="Weekly window" frac={a.sevenD} reset={a.sevenDReset} />
        </div>
      ) : (
        <div className="mt-3 text-[13.5px] text-muted">No window reading yet — the gateway reads each login every few minutes.</div>
      )}

      <div className="mt-5 border-t border-line pt-3.5">
        <div className="flex items-baseline justify-between gap-3">
          <div className="text-[12px] font-medium uppercase tracking-[0.08em] text-faint">Who spent the week</div>
          {a.weighted > 0 && <div className="text-[12px] tabular-nums text-faint">{fmtNum(a.weighted)} tokens · {PERIOD_LABEL[period]}</div>}
        </div>
        {people.length === 0 ? (
          <div className="mt-2 text-[13.5px] text-muted">Nobody ran it in the {PERIOD_LABEL[period]}.</div>
        ) : (
          <div className="mt-1 divide-y divide-line/60">
            {people.map((p) => <ShareRow key={p.id} name={p.name} detail={modelSplit(p.byModel)} ofWeekly={p.ofWeekly} value={p.weighted} />)}
          </div>
        )}
      </div>
    </div>
  );
}

// --- board -----------------------------------------------------------------

const REFRESH_MS = 12000;

export function UsageBoard() {
  const [period, setPeriod] = useState<Period>("week");
  const [accounts, setAccounts] = useState<AccountUsage[]>([]);
  const [people, setPeople] = useState<Subject[]>([]);
  const [asOf, setAsOf] = useState("");
  const [err, setErr] = useState("");
  const periodRef = useRef(period);
  periodRef.current = period;

  useEffect(() => {
    let live = true;
    const load = () => {
      const asked = periodRef.current;
      Promise.all([
        api<{ accounts: AccountUsage[]; asOf: string }>("GET", `/api/usage/accounts?window=${asked}`),
        api<Board>("GET", `/api/usage/people?window=${asked}`),
      ])
        .then(([a, p]) => {
          if (!live || asked !== periodRef.current) return; // a slower answer for a period since left
          setErr("");
          setAccounts(a.accounts || []);
          setPeople(p.subjects || []);
          setAsOf(a.asOf || "");
        })
        .catch((e) => live && setErr(e.message)); // keep last-known data on a transient failure
    };
    load();
    const id = setInterval(load, REFRESH_MS);
    return () => {
      live = false;
      clearInterval(id);
    };
  }, [period]);

  const teamTotal = people.reduce((a, p) => a + p.weighted, 0);
  const ranked = [...people].sort((a, b) => b.weighted - a.weighted);
  const fresh = agoFrom(asOf);

  return (
    <div>
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-[28px] font-bold tracking-tight">Usage</h1>
          <div className="mt-1 text-[15px] text-muted">
            Per account · last request through the gateway {fresh.text.replace(/^as of /, "")}
          </div>
        </div>
        <Seg value={period} onChange={setPeriod} options={[["day", "Day"], ["week", "Week"], ["month", "Month"]]} />
      </div>

      {err && <div className="mt-4 rounded-xl border border-crit/30 bg-crit/5 px-4 py-2.5 text-[14px] text-crit">{err}</div>}

      {accounts.length === 0 ? (
        <div className="mt-6 rounded-2xl border border-dashed border-line px-6 py-8 text-center text-[15px] text-muted">
          No account with a login yet. Add one from a machine's clawdh page and it shows here.
        </div>
      ) : (
        <div className="mt-6 flex flex-col gap-3">
          {accounts.map((a) => <AccountCard key={a.accountId} a={a} period={period} />)}
        </div>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px] text-faint">
        {([["#46C08A", "room left"], ["#E0A83E", "75% used"], ["#E05C53", "95% used"]] as const).map(([c, l]) => (
          <span key={l} className="inline-flex items-center gap-1.5"><span className="h-2 w-4 rounded-full" style={{ background: c }} />{l}</span>
        ))}
        <span className="inline-flex items-center gap-1.5"><span className="h-2 w-4 rounded-full" style={{ background: SHARE }} />one person's slice</span>
      </div>

      {ranked.length > 0 && (
        <>
          <h2 className="mb-1 mt-10 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">Everyone, across all accounts</h2>
          <div className="rounded-2xl border border-line bg-raised px-5 py-2">
            <div className="flex items-baseline justify-between border-b border-line py-2 text-[12px] text-faint">
              <span>{PERIOD_LABEL[period]}</span>
              <span className="tabular-nums">{fmtNum(teamTotal)} total</span>
            </div>
            <div className="divide-y divide-line/60">
              {ranked.map((p) => <TokenRow key={p.id} name={p.name} detail={modelSplit(p.byModel)} value={p.weighted} most={ranked[0]?.weighted || 0} />)}
            </div>
          </div>
        </>
      )}

      <div className="mt-3 text-[12px] text-faint">Every % is a share of that account's real weekly allowance, as Claude reports it.</div>
    </div>
  );
}
