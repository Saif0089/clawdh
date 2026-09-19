import { useEffect, useRef, useState } from "react";
import { motion } from "framer-motion";
import { api, Job } from "./api";
import { when } from "./format";
import { CopyButton, Mono } from "./Copy";

// The remote-help panel for one machine. It can be asked three read-only
// things, all scoped to what is the panel's business: check its health, list
// the sessions that ran through a *shared* account, and fetch one of their
// transcripts. The machine decides scope from its own ledger, so a person's own
// sessions — their personal login, a local account they manage themselves — are
// never listed or sent, whatever is asked. Answers arrive on the machine's next
// check-in (~30s), so this polls while it is open.
export function DeviceJobs({ deviceId, deviceName, onClose }: { deviceId: string; deviceName: string; onClose: () => void }) {
  const [jobs, setJobs] = useState<Job[]>([]);
  const [manual, setManual] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const timer = useRef<ReturnType<typeof setInterval> | null>(null);

  const load = () =>
    api<{ jobs: Job[] }>("GET", `/api/devices/${deviceId}/jobs`)
      .then((r) => setJobs(r.jobs || []))
      .catch((e) => setErr(e.message));

  useEffect(() => {
    load();
    timer.current = setInterval(load, 4000);
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    window.addEventListener("keydown", onKey);
    return () => {
      if (timer.current) clearInterval(timer.current);
      window.removeEventListener("keydown", onKey);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const ask = async (kind: string, params?: string) => {
    setBusy(true);
    setErr("");
    try {
      await api("POST", `/api/devices/${deviceId}/jobs`, { kind, params });
      await load();
    } catch (e: any) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  const pending = jobs.some((j) => j.status === "pending");
  const awaiting = jobs.some((j) => j.status === "awaiting");

  return (
    // A full-height sheet from the right: a workspace beside the People list,
    // not a popup over it — the answers can be long, and one stays open while
    // the machine's owner decides.
    <motion.div
      className="fixed inset-0 z-50 flex justify-end"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.15 }}
      onMouseDown={(e) => e.target === e.currentTarget && onClose()}
    >
      <div className="absolute inset-0 bg-black/55 backdrop-blur-[2px]" />
      <motion.div
        initial={{ x: 40, opacity: 0 }}
        animate={{ x: 0, opacity: 1 }}
        exit={{ x: 40, opacity: 0 }}
        transition={{ duration: 0.22, ease: [0.2, 0.8, 0.2, 1] }}
        role="dialog"
        aria-label={`Remote help — ${deviceName}`}
        className="relative flex h-full w-full max-w-3xl flex-col border-l border-line bg-raised p-5 shadow-2xl sm:p-7"
      >
        <div className="flex items-start gap-3">
          <div className="min-w-0">
            <div className="text-[12.5px] font-semibold uppercase tracking-[0.06em] text-primary">Remote help</div>
            <h2 className="mt-0.5 truncate text-xl font-semibold tracking-tight">{deviceName}</h2>
            <p className="mt-1 text-[13.5px] leading-snug text-muted">
              Its owner turned remote help on. It only ever shares what ran on a <span className="text-ink">shared</span> account — the owner's own sessions stay private — and anything sent here waits for their OK first.
            </p>
          </div>
          <button onClick={onClose} aria-label="Close" className="ml-auto shrink-0 rounded-lg px-2 py-1 text-[15px] text-faint hover:bg-raised-2 hover:text-ink">
            ✕
          </button>
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-2">
          <Action busy={busy} onClick={() => ask("diagnose")}>Check health</Action>
          <Action busy={busy} onClick={() => ask("sessions")}>List shared sessions</Action>
          <form
            className="flex min-w-0 flex-1 items-center gap-2 basis-full sm:basis-auto"
            onSubmit={(e) => {
              e.preventDefault();
              if (manual.trim()) ask("transcript", manual.trim());
            }}
          >
            <input
              value={manual}
              onChange={(e) => setManual(e.target.value)}
              placeholder="…or paste a session id"
              className="min-w-0 flex-1 rounded-lg border border-line bg-sunken px-3 py-2 font-mono text-[13px] outline-none focus:border-primary/60"
            />
            <button type="submit" disabled={busy || !manual.trim()} className="shrink-0 rounded-lg border border-line bg-raised-2 px-3 py-2 text-[14px] hover:border-primary/50 disabled:opacity-40">
              Transcript
            </button>
          </form>
        </div>

        {err && <div className="mt-3 rounded-lg border border-crit/30 bg-crit/10 px-3 py-2 text-[13.5px] text-crit">{err}</div>}

        <div className="mt-5 min-h-0 flex-1 overflow-y-auto pr-1">
          <div className="mb-2 flex items-center gap-2 text-[12.5px] font-semibold uppercase tracking-[0.06em] text-muted">
            Answers
            {pending && (
              <span className="flex items-center gap-1.5 text-[12px] font-normal normal-case text-faint">
                <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-warn" /> waiting for {deviceName} to check in…
              </span>
            )}
            {!pending && awaiting && (
              <span className="flex items-center gap-1.5 text-[12px] font-normal normal-case text-faint">
                <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-warn" /> waiting for its owner to approve…
              </span>
            )}
          </div>
          {jobs.length === 0 && (
            <div className="rounded-xl border border-dashed border-line px-4 py-6 text-center text-[14px] text-faint">
              Nothing asked yet. Check its health, or list its shared-account sessions and open a transcript.
            </div>
          )}
          <div className="flex flex-col gap-3">
            {jobs.map((j) => (
              <JobCard key={j.id} job={j} deviceName={deviceName} onTranscript={(id) => ask("transcript", id)} />
            ))}
          </div>
        </div>
      </motion.div>
    </motion.div>
  );
}

function Action({ busy, onClick, children }: { busy: boolean; onClick: () => void; children: React.ReactNode }) {
  return (
    <button disabled={busy} onClick={onClick} className="rounded-lg border border-line bg-raised-2 px-3.5 py-2 text-[14px] font-medium transition-colors hover:border-primary/50 disabled:opacity-50">
      {children}
    </button>
  );
}

// JobCard renders one request and its answer, each kind in the form that reads
// best — never as a raw blob. The raw answer is always one click away.
function JobCard({ job: j, deviceName, onTranscript }: { job: Job; deviceName: string; onTranscript: (id: string) => void }) {
  const label = j.kind === "diagnose" ? "Health check" : j.kind === "sessions" ? "Shared sessions" : j.kind === "transcript" ? "Transcript" : j.kind;
  return (
    <div className="rounded-xl border border-line bg-sunken p-3.5">
      <div className="flex min-w-0 items-center gap-2 text-[14px]">
        <span className="font-semibold">{label}</span>
        {j.kind === "transcript" && j.params && <Mono text={j.params} className="min-w-0" />}
        <JobBadge status={j.status} />
        <span className="ml-auto shrink-0 text-[12.5px] text-faint">{when(j.createdAt)}</span>
      </div>
      {j.status === "pending" && <div className="mt-2 text-[13px] text-faint">Waiting for {deviceName} to check in (up to ~30s)…</div>}
      {j.status === "awaiting" && (
        <div className="mt-2 rounded-lg border border-warn/30 bg-warn/10 px-3 py-2 text-[13px] text-warn">
          Held on {deviceName} until its owner allows it. They've been notified; this stays here until they decide (or an hour passes).
        </div>
      )}
      {j.status === "denied" && <div className="mt-2 text-[13.5px] text-muted">{j.result || "The owner declined this request."}</div>}
      {j.status === "error" && <div className="mt-2 text-[13.5px] text-crit">{j.result || "That did not work."}</div>}
      {j.status === "done" && j.result && j.kind === "diagnose" && <DiagnoseView raw={j.result} />}
      {j.status === "done" && j.result && j.kind === "sessions" && <SessionsView raw={j.result} onTranscript={onTranscript} />}
      {j.status === "done" && j.result && j.kind === "transcript" && <TranscriptView raw={j.result} sessionID={j.params || "session"} />}
      {j.status === "done" && j.result && !["diagnose", "sessions", "transcript"].includes(j.kind) && <Raw text={j.result} />}
    </div>
  );
}

// DiagnoseView shows the health report as labelled rows instead of a text dump.
function DiagnoseView({ raw }: { raw: string }) {
  const lines = raw.split("\n").filter((l) => l.trim());
  return (
    <div className="mt-2.5">
      <div className="overflow-hidden rounded-lg border border-line bg-ground">
        {lines.map((l, i) => {
          const at = l.indexOf(":");
          const key = at > 0 ? l.slice(0, at).trim() : "";
          const val = at > 0 ? l.slice(at + 1).trim() : l.trim();
          const bad = /NOT running|not found|unreachable|failed|error/i.test(val);
          return (
            <div key={i} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 border-b border-line px-3 py-1.5 text-[13px] last:border-b-0">
              {key && <span className="w-32 shrink-0 text-faint">{key}</span>}
              <span className={`min-w-0 break-words ${bad ? "text-crit" : "text-ink"} ${key ? "" : "font-medium"}`}>{val}</span>
            </div>
          );
        })}
      </div>
      <div className="mt-1.5 flex justify-end">
        <CopyButton text={raw} label="Copy report" />
      </div>
    </div>
  );
}

type SessionRow = { id: string; share: string; project: string; size: number; mod: string };

// SessionsView lists the shared-account sessions as rows with a one-click
// transcript, and repeats the privacy rule so it is never a surprise.
function SessionsView({ raw, onTranscript }: { raw: string; onTranscript: (id: string) => void }) {
  let d: { sessions: SessionRow[]; note?: string };
  try {
    d = JSON.parse(raw);
  } catch {
    return <Raw text={raw} />;
  }
  const rows = d.sessions || [];
  return (
    <div className="mt-2.5">
      {rows.length === 0 ? (
        <div className="rounded-lg border border-line bg-ground px-3 py-3 text-[13.5px] text-faint">No sessions have run through a shared account on this machine yet.</div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-line bg-ground">
          <div className="hidden grid-cols-[1fr_auto_auto_auto] gap-3 border-b border-line px-3 py-1.5 text-[11.5px] font-semibold uppercase tracking-[0.06em] text-faint sm:grid">
            <span>Session · project</span>
            <span>Account</span>
            <span className="text-right">Size</span>
            <span />
          </div>
          {rows.map((s) => (
            <div key={s.id} className="grid grid-cols-1 gap-2 border-b border-line px-3 py-2 last:border-b-0 sm:grid-cols-[1fr_auto_auto_auto] sm:items-center sm:gap-3">
              <div className="min-w-0">
                <Mono text={s.id} className="max-w-full" />
                <div className="mt-0.5 truncate text-[12.5px] text-faint" title={s.project}>
                  {s.project} · {s.mod}
                </div>
              </div>
              <span className="w-fit rounded-full border border-primary/40 bg-primary/10 px-2 py-[1px] text-[12px] text-primary">{s.share}</span>
              <span className="text-[12.5px] tabular-nums text-faint sm:text-right">{humanBytes(s.size)}</span>
              <button onClick={() => onTranscript(s.id)} className="w-fit rounded-md border border-line bg-raised-2 px-2.5 py-1 text-[12.5px] text-ink hover:border-primary/50">
                Transcript
              </button>
            </div>
          ))}
        </div>
      )}
      {d.note && <div className="mt-1.5 text-[12px] text-faint">{d.note}</div>}
    </div>
  );
}

// TranscriptView renders a Claude Code transcript as readable turns, with the
// raw JSONL a toggle away, and copy/download for either.
function TranscriptView({ raw, sessionID }: { raw: string; sessionID: string }) {
  const [mode, setMode] = useState<"read" | "raw">("read");
  const truncated = raw.startsWith("…(truncated");
  const body = truncated ? raw.slice(raw.indexOf("\n") + 1) : raw;
  const turns = parseTurns(body);
  const download = () => {
    const a = document.createElement("a");
    a.href = "data:text/plain;charset=utf-8," + encodeURIComponent(body);
    a.download = `${sessionID}.jsonl`;
    a.click();
  };
  return (
    <div className="mt-2.5">
      <div className="mb-1.5 flex flex-wrap items-center gap-2 text-[12.5px] text-faint">
        <span>
          {turns ? `${turns.length} turn${turns.length === 1 ? "" : "s"}` : "transcript"}
          {truncated ? " · showing the last 512 KB" : ""}
        </span>
        <div className="ml-auto flex items-center gap-1.5">
          {turns && (
            <div className="inline-flex rounded-md border border-line bg-raised-2 p-0.5">
              {(["read", "raw"] as const).map((m) => (
                <button key={m} onClick={() => setMode(m)} className={`rounded px-2 py-0.5 text-[12px] ${mode === m ? "bg-raised text-ink" : "text-muted hover:text-ink"}`}>
                  {m === "read" ? "Readable" : "Raw"}
                </button>
              ))}
            </div>
          )}
          <CopyButton text={body} />
          <button onClick={download} className="rounded-md border border-line bg-raised-2 px-2.5 py-1 text-[12.5px] text-muted hover:border-primary/50 hover:text-ink">
            Download
          </button>
        </div>
      </div>
      {turns && mode === "read" ? (
        <div className="max-h-80 space-y-2 overflow-auto rounded-lg border border-line bg-ground p-3">
          {turns.map((t, i) => (
            <div key={i} className="text-[13px] leading-relaxed">
              <span className={`mr-2 rounded px-1.5 py-[1px] text-[11px] font-semibold uppercase tracking-wide ${t.role === "user" ? "bg-primary/15 text-primary" : "bg-raised-2 text-muted"}`}>{t.role}</span>
              <span className="whitespace-pre-wrap break-words text-ink/90">{t.text}</span>
            </div>
          ))}
        </div>
      ) : (
        <Raw text={body} />
      )}
    </div>
  );
}

// parseTurns pulls readable user/assistant text out of transcript JSONL; null if
// nothing recognisable, so the caller falls back to raw.
function parseTurns(raw: string): { role: string; text: string }[] | null {
  const out: { role: string; text: string }[] = [];
  for (const line of raw.split("\n")) {
    if (!line.trim().startsWith("{")) continue;
    try {
      const o = JSON.parse(line);
      const r = o.message?.role || o.type;
      const role = r === "user" ? "user" : r === "assistant" ? "assistant" : null;
      if (!role) continue;
      const c = o.message?.content ?? o.content;
      let text = "";
      if (typeof c === "string") text = c;
      else if (Array.isArray(c)) text = c.filter((p: any) => p && p.type === "text" && typeof p.text === "string").map((p: any) => p.text).join("\n");
      if (text.trim()) out.push({ role, text: text.trim() });
    } catch {
      /* not JSON: skip */
    }
  }
  return out.length ? out : null;
}

function Raw({ text }: { text: string }) {
  return <pre className="mt-2 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-line bg-ground p-3 font-mono text-[12.5px] leading-relaxed text-muted">{text}</pre>;
}

function humanBytes(n: number): string {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
  return n + " B";
}

function JobBadge({ status }: { status: string }) {
  const map: Record<string, [string, string]> = {
    pending: ["text-warn border-warn/40", "sent"],
    awaiting: ["text-warn border-warn/40", "waiting for approval"],
    done: ["text-ok border-ok/40", "answered"],
    error: ["text-crit border-crit/40", "failed"],
    denied: ["text-muted border-line", "declined"],
  };
  const [cls, label] = map[status] || ["text-faint border-line", status];
  return <span className={`shrink-0 rounded-full border px-2 py-[1px] text-[11px] ${cls}`}>{label}</span>;
}
