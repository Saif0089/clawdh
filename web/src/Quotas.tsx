import { useEffect, useState } from "react";
import { motion } from "framer-motion";
import { api, Limit } from "./api";
import { untilReset } from "./format";

// A quota here is one thing: a ceiling on how full an account's weekly window
// may be — Claude's own /usage number — before the gateway turns someone away.

interface LimitRow {
  limit: Limit;
  name: string;
  fraction?: number; // how far into the ceiling: window ÷ ceiling
  resetAt?: string; // when the window it is checked against rolls over
  window?: number; // how full that window is (0..1)
  windowAccount?: string; // …and whose window that is
}

// standing spells out what the bar means — "Team Max's weekly window is 45%
// full · ceiling 60%" — so the ceiling's fill is never confused with the
// window's fill. Both live on this board and both are named.
function standing(r: LimitRow): string {
  const l = r.limit;
  const parts: string[] = [];
  if (r.window == null) parts.push("no window reading yet for " + (l.subjectType === "account" ? "this account" : "the accounts they can use"));
  else parts.push(`${r.windowAccount ? r.windowAccount + "'s" : "the"} weekly window is ${Math.round(r.window * 100)}% full · ceiling ${Math.round(l.maxPercent * 100)}%`);
  const reset = untilReset(r.resetAt);
  if (reset) parts.push(reset);
  return parts.join(" · ");
}

// tone maps the standing against the ceiling to a colour and a word: green
// with room, amber from 90%, red once the window is past it and the gateway
// is turning the subject away.
function tone(frac: number): { color: string; label: string } {
  if (frac >= 1) return { color: "#E05C53", label: "past the ceiling — turned away" };
  if (frac >= 0.9) return { color: "#E0A83E", label: "nearly at the ceiling" };
  return { color: "#46C08A", label: "under the ceiling" };
}

export function Quotas() {
  const [rows, setRows] = useState<LimitRow[]>([]);
  const [people, setPeople] = useState<{ id: string; name: string }[]>([]);
  const [accounts, setAccounts] = useState<{ id: string; name: string }[]>([]);
  const [subject, setSubject] = useState("");
  const [percent, setPercent] = useState("");
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

  // Nothing picked yet: the first person, else the first account.
  useEffect(() => {
    if (subject) return;
    if (people[0]) setSubject("person:" + people[0].id);
    else if (accounts[0]) setSubject("account:" + accounts[0].id);
  }, [people, accounts, subject]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    const p = parseFloat(percent);
    if (!(p > 0 && p <= 100)) return setErr("Enter a percent between 0 and 100.");
    if (!subject) return setErr("Pick who the ceiling applies to.");
    const [subjectType, subjectId] = subject.split(":", 2);
    try {
      await api("POST", "/api/limits", { subjectType, subjectId, maxPercent: p }); // 0..100; the server normalises
      setPercent("");
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
      <div className="mt-1 text-[15px] leading-relaxed text-muted">
        A quota is a <b className="text-ink">ceiling</b> on how full an account's weekly window may be — Claude's own number — before the gateway turns someone away. On a person, it holds whichever account they are using: lend an account with a 60% ceiling and they can never eat into the top 40%. On an account, it holds everyone using it — a reserve. Nothing is blocked until the gateway has a reading.
      </div>

      <h2 className="mb-2.5 mt-8 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">In effect</h2>
      {rows.length === 0 && (
        <div className="rounded-2xl border border-dashed border-line px-6 py-8 text-center text-[15px] text-muted">
          No ceilings yet — everyone shares the subscription freely. Set one below.
        </div>
      )}
      <div className="flex flex-col gap-2">
        {rows.map((r) => {
          const frac = r.fraction || 0;
          const t = tone(frac);
          return (
            <div key={r.limit.id} className="rounded-2xl border border-line bg-raised p-4">
              <div className="flex flex-wrap items-start gap-x-3 gap-y-2">
                <div className="min-w-0 flex-1 basis-[200px]">
                  <div className="truncate text-[16px] font-semibold">{r.name}</div>
                  <div className="text-[14px] text-muted">stops at {Math.round(r.limit.maxPercent * 100)}% of the weekly window</div>
                </div>
                <div className="flex items-center gap-3">
                  <div className="text-[15px] font-semibold tabular-nums" style={{ color: t.color }}>{t.label}</div>
                  <button onClick={() => remove(r.limit.id)} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-faint transition-colors hover:border-crit/50 hover:text-crit">Remove</button>
                </div>
              </div>
              <div className="mt-3 h-2 overflow-hidden rounded-full bg-sunken">
                <motion.div className="h-full rounded-full" style={{ background: t.color }} initial={{ width: 0 }} animate={{ width: `${Math.min(frac, 1) * 100}%` }} transition={{ duration: 0.5, ease: [0.2, 0.8, 0.2, 1] }} />
              </div>
              <div className="mt-1.5 text-[13px] tabular-nums text-faint">{standing(r)}</div>
            </div>
          );
        })}
      </div>

      <h2 className="mb-2.5 mt-8 text-[13px] font-semibold uppercase tracking-[0.08em] text-muted">Set a ceiling</h2>
      <form onSubmit={submit} className="rounded-2xl border border-line bg-raised p-5">
        <div className="flex flex-wrap items-center gap-2.5">
          <select value={subject} onChange={(e) => setSubject(e.target.value)} className="rounded-lg border border-line bg-sunken px-3 py-2.5 text-[15px] outline-none focus:border-primary/60">
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
          <div className="flex items-center gap-2 rounded-lg border border-line bg-sunken px-3 py-1.5">
            <span className="text-[15px] text-muted">stops at</span>
            <input value={percent} onChange={(e) => setPercent(e.target.value)} type="number" min={1} max={100} step={1} placeholder="60" className="w-16 bg-transparent text-[16px] font-semibold tabular-nums outline-none" />
            <span className="text-[15px] text-muted">% of the weekly window</span>
          </div>
          <button type="submit" className="rounded-lg bg-primary px-5 py-2.5 text-[15px] font-semibold text-sunken transition-transform active:scale-[0.98]">Set ceiling</button>
        </div>
      </form>
      {err && <div className="mt-2 text-[14px] text-crit">{err}</div>}
    </div>
  );
}
