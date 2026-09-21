import { useEffect, useState } from "react";
import { motion } from "framer-motion";
import { api } from "./api";
import { fmtNum, untilReset } from "./format";

interface LimitRow {
  limit: { id: string; subjectType: string; subjectId: string; windowKind: string; maxWeighted?: number; maxCostUsd?: number; maxPercent?: number };
  name: string;
  fraction?: number;
  resetAt?: string;
  window?: number; // for a ceiling: how full the weekly window it is checked against is (0..1)
  windowAccount?: string; // …and whose window that is
}

function capLabel(l: LimitRow["limit"]): string {
  const caps: string[] = [];
  if (l.maxPercent) caps.push(`stops at ${Math.round(l.maxPercent * 100)}% of the weekly window`);
  if (l.maxWeighted) caps.push(fmtNum(l.maxWeighted) + " tokens · per " + l.windowKind);
  if (l.maxCostUsd) caps.push("$" + l.maxCostUsd + " · per " + l.windowKind);
  return caps.join(" / ");
}

const periodWord: Record<string, string> = { day: "today", week: "this week", month: "this month" };

// usedLabel spells out what the bar means, in the cap's own unit — "17% of the
// weekly window used · cap 25%" — so a quota's fill is never confused with the
// window's fill. Two percentages live on this board (how full the quota is,
// how full the account's window is) and both are named.
function usedLabel(r: LimitRow): string {
  const l = r.limit;
  const frac = r.fraction || 0;
  const parts: string[] = [];
  const period = periodWord[l.windowKind] || "per " + l.windowKind;
  if (l.maxPercent) {
    // A ceiling: the number is the account's own window, so name the account.
    if (r.window == null) parts.push("no window reading yet for " + (l.subjectType === "account" ? "this account" : "the accounts they can use"));
    else parts.push(`${r.windowAccount ? r.windowAccount + "'s" : "the"} weekly window is ${Math.round(r.window * 100)}% full · ceiling ${Math.round(l.maxPercent * 100)}%`);
  }
  if (l.maxWeighted) parts.push(`${fmtNum(frac * l.maxWeighted)} of ${fmtNum(l.maxWeighted)} tokens ${period}`);
  if (l.maxCostUsd) parts.push(`$${(frac * l.maxCostUsd).toFixed(2)} of $${l.maxCostUsd} ${period}`);
  const reset = untilReset(r.resetAt);
  if (reset) parts.push(reset);
  return parts.join(" · ");
}

// usageTone maps how full a quota is to a colour and a word: green under 75%,
// amber approaching (75–95%), red near or over the cap — the 75/95 marks the
// gateway warns and blocks at. The word always says "of quota", because the
// board also shows how full the underlying window is.
function usageTone(frac: number, ceiling: boolean): { color: string; label: string } {
  if (ceiling) {
    // The headline is the standing against the ceiling, in the ceiling's own words.
    if (frac >= 1) return { color: "#E05C53", label: "past the ceiling — turned away" };
    if (frac >= 0.9) return { color: "#E0A83E", label: "nearly at the ceiling" };
    return { color: "#46C08A", label: "under the ceiling" };
  }
  if (frac >= 1) return { color: "#E05C53", label: "over the cap" };
  if (frac >= 0.95) return { color: "#E05C53", label: `${Math.round(frac * 100)}% of quota — at the cap` };
  if (frac >= 0.75) return { color: "#E0A83E", label: `${Math.round(frac * 100)}% of quota — approaching` };
  return { color: "#46C08A", label: `${Math.round(frac * 100)}% of quota` };
}

type Mode = "percent" | "weighted" | "cost";

export function Quotas() {
  const [rows, setRows] = useState<LimitRow[]>([]);
  const [people, setPeople] = useState<{ id: string; name: string }[]>([]);
  const [accounts, setAccounts] = useState<{ id: string; name: string }[]>([]);
  const [mode, setMode] = useState<Mode>("percent");
  const [subject, setSubject] = useState("org");
  const [windowKind, setWindowKind] = useState("week");
  const [percent, setPercent] = useState("");
  const [weighted, setWeighted] = useState("");
  const [cost, setCost] = useState("");
  const [err, setErr] = useState("");

  const load = () => {
    Promise.all([api<{ limits: LimitRow[] }>("GET", "/api/limits"), api<{ people: { id: string; name: string }[]; accounts: { id: string; name: string }[] }>("GET", "/api/panel")])
      .then(([l, p]) => {
        setRows(l.limits || []);
        setPeople(p.people || []);
        setAccounts(p.accounts || []);
      })
      .catch((e) => setErr(e.message));
  };
  useEffect(() => {
    load();
    const id = setInterval(load, 12000);
    return () => clearInterval(id);
  }, []);

  // "% of weekly" can't apply to the whole team, so when that mode is on and the
  // subject is still the team, move to the first person (or account).
  useEffect(() => {
    if (mode === "percent" && subject === "org") {
      if (people[0]) setSubject("person:" + people[0].id);
      else if (accounts[0]) setSubject("account:" + accounts[0].id);
    }
  }, [mode, people, accounts, subject]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    const body: any = { windowKind };
    if (subject === "org") body.subjectType = "org";
    else if (subject.startsWith("account:")) {
      body.subjectType = "account";
      body.subjectId = subject.slice("account:".length);
    } else {
      body.subjectType = "person";
      body.subjectId = subject.slice("person:".length);
    }
    if (mode === "percent") {
      const p = parseFloat(percent);
      if (!(p > 0 && p <= 100)) return setErr("Enter a percent between 0 and 100.");
      if (subject === "org") return setErr("A window ceiling applies to a person or an account — pick one above.");
      body.maxPercent = p; // 0..100; the server normalises + forces a weekly window
    } else if (mode === "weighted") {
      const w = parseFloat(weighted);
      if (!(w > 0)) return setErr("Enter a token cap.");
      body.maxWeighted = w;
    } else {
      const c = parseFloat(cost);
      if (!(c > 0)) return setErr("Enter a USD cap.");
      body.maxCostUsd = c;
    }
    try {
      await api("POST", "/api/limits", body);
      setPercent("");
      setWeighted("");
      setCost("");
      load();
    } catch (e: any) {
      setErr(e.message);
    }
  };

  const remove = async (id: string) => {
    try {
      await api("DELETE", "/api/limits/" + id);
      load();
    } catch (e: any) {
      setErr(e.message);
    }
  };

  return (
    <div>
      <h1 className="text-[28px] font-bold tracking-tight">Quotas</h1>
      <div className="mt-1 text-[15px] text-muted">Two kinds of cap. A <b className="text-ink">window ceiling</b> stops someone once an account's weekly window is fuller than a percentage — Claude's own number. A <b className="text-ink">token or cost</b> cap limits what the gateway meters for a person, an account, or the whole team per day, week, or month. Over either, the gateway turns them away the way a real spend limit does.</div>

      <h2 className="mb-2.5 mt-8 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">In effect</h2>
      {rows.length === 0 && (
        <div className="rounded-2xl border border-dashed border-line px-6 py-8 text-center text-[15px] text-muted">
          No quotas yet — everyone shares the subscription freely. Set one below.
        </div>
      )}
      <div className="flex flex-col gap-2">
        {rows.map((r) => {
          const frac = r.fraction || 0;
          const tone = usageTone(frac, !!r.limit.maxPercent);
          return (
            <div key={r.limit.id} className="rounded-2xl border border-line bg-raised p-4">
              <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
                <div className="min-w-0 flex-1 basis-[200px]">
                  <div className="truncate text-[16px] font-semibold">{r.name}</div>
                  <div className="text-[14px] text-muted">{capLabel(r.limit)}</div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="text-[15px] font-semibold tabular-nums" style={{ color: tone.color }}>{tone.label}</div>
                  <button onClick={() => remove(r.limit.id)} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-faint transition-colors hover:border-crit/50 hover:text-crit">Remove</button>
                </div>
              </div>
              <div className="mt-3 h-2 overflow-hidden rounded-full bg-sunken">
                <motion.div className="h-full rounded-full" style={{ background: tone.color }} initial={{ width: 0 }} animate={{ width: `${Math.min(frac, 1) * 100}%` }} transition={{ duration: 0.5, ease: [0.2, 0.8, 0.2, 1] }} />
              </div>
              <div className="mt-1.5 text-[13px] tabular-nums text-faint">{usedLabel(r)}</div>
            </div>
          );
        })}
      </div>

      <h2 className="mb-2.5 mt-8 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">Set a quota</h2>
      <form onSubmit={submit} className="rounded-2xl border border-line bg-raised p-5">
        {/* Cap type — % of the weekly window leads; the raw caps are for anyone who wants them. */}
        <div className="inline-flex rounded-xl border border-line bg-sunken p-1">
          {([["percent", "Window ceiling"], ["weighted", "Tokens"], ["cost", "Cost ($)"]] as [Mode, string][]).map(([m, label]) => (
            <button type="button" key={m} onClick={() => setMode(m)} className={`rounded-lg px-3.5 py-1.5 text-[14px] font-medium transition-colors ${mode === m ? "bg-raised-2 text-ink" : "text-muted hover:text-ink"}`}>{label}</button>
          ))}
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-2.5">
          <select value={subject} onChange={(e) => setSubject(e.target.value)} className="rounded-lg border border-line bg-sunken px-3 py-2.5 text-[15px] outline-none focus:border-primary/60">
            {mode !== "percent" && <option value="org">The whole team</option>}
            {people.length > 0 && (
              <optgroup label="People">
                {people.map((p) => (
                  <option key={p.id} value={"person:" + p.id}>{p.name}</option>
                ))}
              </optgroup>
            )}
            {accounts.length > 0 && (
              <optgroup label="Accounts">
                {accounts.map((a) => (
                  <option key={a.id} value={"account:" + a.id}>{a.name}</option>
                ))}
              </optgroup>
            )}
          </select>

          {mode === "percent" ? (
            <div className="flex items-center gap-2 rounded-lg border border-line bg-sunken px-3 py-1.5">
              <span className="text-[15px] text-muted">stop at</span>
              <input value={percent} onChange={(e) => setPercent(e.target.value)} type="number" min={1} max={100} step={1} placeholder="60" className="w-16 bg-transparent text-[16px] font-semibold tabular-nums outline-none" />
              <span className="text-[15px] text-muted">% of the weekly window</span>
            </div>
          ) : (
            <>
              <select value={windowKind} onChange={(e) => setWindowKind(e.target.value)} className="rounded-lg border border-line bg-sunken px-3 py-2.5 text-[15px] outline-none focus:border-primary/60">
                <option value="day">per day</option>
                <option value="week">per week</option>
                <option value="month">per month</option>
              </select>
              {mode === "weighted" ? (
                <input value={weighted} onChange={(e) => setWeighted(e.target.value)} type="number" min={0} placeholder="max tokens" className="min-w-[190px] flex-1 rounded-lg border border-line bg-sunken px-3 py-2.5 text-[15px] outline-none focus:border-primary/60" />
              ) : (
                <input value={cost} onChange={(e) => setCost(e.target.value)} type="number" min={0} step={0.5} placeholder="max $" className="min-w-[150px] flex-1 rounded-lg border border-line bg-sunken px-3 py-2.5 text-[15px] outline-none focus:border-primary/60" />
              )}
            </>
          )}

          <button type="submit" className="rounded-lg bg-primary px-5 py-2.5 text-[15px] font-semibold text-sunken transition-transform active:scale-[0.98]">Set quota</button>
        </div>
        {mode === "percent" && (
          <div className="mt-2.5 text-[13px] leading-relaxed text-faint">
            A ceiling on how full an account's weekly window may be — Claude's own /usage number, not an estimate of who used what. For a person, they're turned away while any account they use is fuller than this (lend an account to another team with a 60% ceiling and they can never eat into the top 40%). For an account, it holds everyone using it — a reserve. Nothing is blocked until a reading exists.
          </div>
        )}
      </form>
      {err && <div className="mt-2 text-[14px] text-crit">{err}</div>}
    </div>
  );
}
