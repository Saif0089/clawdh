import { useEffect, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { api, Panel, Account, Person, Device } from "./api";
import { ClawIntro } from "./ClawIntro";
import { Tour, HelpFab, adminSteps, gateSteps, introSeen, markIntroSeen } from "./Tour";
import { UsageBoard } from "./UsageBoard";
import { Quotas } from "./Quotas";
import { useDialog } from "./Dialog";
import { DeviceJobs } from "./DeviceJobs";
import { when, untilExpiry } from "./format";

type Ask = ReturnType<typeof useDialog>["ask"];

type Tab = "accounts" | "people" | "usage" | "quotas" | "activity";
type Status = "loading" | "setup" | "gate" | "in";

export default function App() {
  const [intro, setIntro] = useState(true);
  const [status, setStatus] = useState<Status>("loading");
  const [tab, setTab] = useState<Tab>("accounts");
  const [actor, setActor] = useState("");
  const [tour, setTour] = useState(false);
  const reduce = useReducedMotion();
  const seen = useRef(introSeen());

  useEffect(() => {
    api<{ needsSetup: boolean; signedIn: boolean; actor?: string }>("GET", "/api/status")
      .then((s) => {
        setActor(s.actor || "");
        setStatus(s.signedIn ? "in" : s.needsSetup ? "setup" : "gate");
      })
      .catch(() => setStatus("gate"));
  }, []);

  // First run, once the claw-slash has lifted: open the guided tour on its own —
  // the admin track when signed in, a short "what is this" welcome otherwise (so
  // even a first, not-yet-signed-in visit, e.g. incognito, sees the intro). The
  // "?" replays it any time.
  useEffect(() => {
    if (status !== "loading" && !intro && !seen.current) {
      const t = setTimeout(() => setTour(true), reduce ? 200 : 350);
      return () => clearTimeout(t);
    }
  }, [status, intro, reduce]);

  const closeTour = () => {
    setTour(false);
    seen.current = true;
    markIntroSeen();
  };

  return (
    <div className="min-h-full font-sans text-ink">
      <Backdrop />
      <AnimatePresence>{intro && <ClawIntro onDone={() => setIntro(false)} />}</AnimatePresence>
      {status === "loading" ? null : status === "in" ? (
        <Shell tab={tab} setTab={setTab} actor={actor} onSignOut={() => setStatus("gate")} />
      ) : (
        <Gate
          setup={status === "setup"}
          onIn={(name) => {
            setActor(name);
            setStatus("in");
          }}
        />
      )}
      {status !== "loading" && !intro && <HelpFab onClick={() => setTour(true)} />}
      {tour && <Tour steps={status === "in" ? adminSteps : gateSteps} onClose={closeTour} onTab={(t) => setTab(t as Tab)} />}
    </div>
  );
}

// Logo is clawdh's mark: three tapered claw slashes raked across, in the brand
// gradient — the same motif the intro animates.
function Logo({ size = 28 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" fill="none" aria-hidden className="logo-mark shrink-0">
      <defs>
        <linearGradient id="clawgrad" x1="4" y1="2" x2="28" y2="30" gradientUnits="userSpaceOnUse">
          <stop stopColor="#6E8BFF" />
          <stop offset="1" stopColor="#C77DFF" />
        </linearGradient>
      </defs>
      <path pathLength={1} d="M8 4C12.5 9 14.5 17 13.5 28" stroke="url(#clawgrad)" strokeWidth="3.4" strokeLinecap="round" />
      <path pathLength={1} d="M16 3C20.5 9 22.5 18 21.5 29" stroke="url(#clawgrad)" strokeWidth="3.4" strokeLinecap="round" />
      <path pathLength={1} d="M24 5C27 10 28 16.5 27 25" stroke="url(#clawgrad)" strokeWidth="3.4" strokeLinecap="round" opacity="0.9" />
    </svg>
  );
}

// Backdrop is the live wallpaper: slow-drifting aurora blobs + a faint grid,
// fixed behind the whole app (see index.css). Rendered once.
function Backdrop() {
  return (
    <div className="backdrop" aria-hidden>
      <span className="blob a" />
      <span className="blob b" />
      <span className="blob c" />
    </div>
  );
}

// actorKey remembers the name this browser last signed in with — a convenience,
// never an identity: the panel records changes under whatever name is given.
const actorKey = "clawdh:actor";

function Gate({ setup, onIn }: { setup: boolean; onIn: (name: string) => void }) {
  const [name, setName] = useState(() => {
    try {
      return localStorage.getItem(actorKey) || "";
    } catch {
      return "";
    }
  });
  const [pw, setPw] = useState("");
  const [err, setErr] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr("");
    try {
      const r = await api<{ name: string }>("POST", setup ? "/api/setup" : "/api/login", { password: pw, name });
      try {
        localStorage.setItem(actorKey, r.name || name);
      } catch {
        /* private mode */
      }
      onIn(r.name || name);
    } catch (e: any) {
      setErr(e.message);
    }
  };
  return (
    <div className="flex min-h-screen items-center justify-center px-4">
      <motion.form
        onSubmit={submit}
        initial={{ opacity: 0, y: 12 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 1.4, duration: 0.4 }}
        className="w-full max-w-sm rounded-2xl border border-line bg-raised p-8"
      >
        <div className="wordmark flex items-center gap-2.5">
          <Logo size={30} />
          <span className="text-2xl font-bold tracking-tight">clawdh</span>
        </div>
        <div className="mt-2 text-[15px] text-muted">
          {setup ? "Set the panel's password — everyone who runs it will share this one." : "Sign in to the team panel."}
        </div>
        <label className="mt-5 block text-[12.5px] font-medium uppercase tracking-[0.06em] text-faint">Your name</label>
        <input
          autoFocus={!name}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. Hassan"
          maxLength={40}
          autoComplete="name"
          className="mt-1.5 w-full rounded-xl border border-line bg-sunken px-4 py-3 text-[16px] outline-none focus:border-primary/60"
        />
        <div className="mt-1.5 text-[12.5px] leading-snug text-faint">Every change you make is recorded under this name, so the team can see who did what.</div>
        <label className="mt-4 block text-[12.5px] font-medium uppercase tracking-[0.06em] text-faint">{setup ? "New password" : "Password"}</label>
        <input
          type="password"
          autoFocus={!!name}
          value={pw}
          onChange={(e) => setPw(e.target.value)}
          placeholder={setup ? "At least 10 characters" : "Password"}
          autoComplete={setup ? "new-password" : "current-password"}
          className="mt-1.5 w-full rounded-xl border border-line bg-sunken px-4 py-3 text-[16px] outline-none focus:border-primary/60"
        />
        <button className="mt-5 w-full rounded-xl bg-primary py-3 font-semibold text-sunken transition-transform active:scale-[0.98]">
          {setup ? "Create panel" : "Sign in"}
        </button>
        <div className="mt-3 min-h-[1.2em] text-sm text-crit">{err}</div>
      </motion.form>
    </div>
  );
}

function Shell({ tab, setTab, actor, onSignOut }: { tab: Tab; setTab: (t: Tab) => void; actor: string; onSignOut: () => void }) {
  const signOut = async () => {
    try {
      await api("POST", "/api/logout");
    } catch {
      /* ignore */
    }
    onSignOut();
  };
  const tabs: [Tab, string][] = [["accounts", "Accounts"], ["people", "People"], ["usage", "Usage"], ["quotas", "Quotas"], ["activity", "Activity"]];
  return (
    <div className="mx-auto max-w-5xl px-5 py-6 sm:px-8">
      <header className="wordmark flex items-center gap-2.5 pb-4">
        <Logo />
        <span className="text-[22px] font-bold tracking-tight">clawdh</span>
        <span className="rounded-full border border-line bg-raised px-2.5 py-0.5 text-[12px] font-medium text-muted">Team panel</span>
        <div className="ml-auto flex min-w-0 items-center gap-3">
          {actor && (
            <span className="hidden min-w-0 items-center gap-1.5 text-[13.5px] text-muted sm:flex" title="Your changes are recorded under this name">
              <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/15 text-[11.5px] font-semibold text-primary">{actor.slice(0, 1).toUpperCase()}</span>
              <span className="truncate">{actor}</span>
            </span>
          )}
          <button onClick={signOut} className="shrink-0 text-[14px] text-faint transition-colors hover:text-ink">Sign out</button>
        </div>
      </header>
      {/* On a phone the strip scrolls sideways under the finger (no page overflow); the
          page's side gutter is kept so the first tab lines up with the content. */}
      <nav className="scrollbar-none -mx-5 flex gap-1 overflow-x-auto border-b border-line px-5 sm:mx-0 sm:px-0">
        {tabs.map(([t, label]) => (
          <button key={t} onClick={() => setTab(t)} className="relative shrink-0 px-3.5 pb-3 pt-1 text-[16px] font-medium transition-colors">
            <span className={t === tab ? "text-ink" : "text-muted hover:text-ink"}>{label}</span>
            {t === tab && <motion.span layoutId="tab-underline" className="absolute inset-x-2 -bottom-px h-0.5 rounded-full bg-primary" transition={{ type: "spring", stiffness: 500, damping: 38 }} />}
          </button>
        ))}
      </nav>
      <motion.div key={tab} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }} className="pt-8">
        {tab === "usage" ? <UsageBoard /> : tab === "quotas" ? <Quotas /> : <PanelTab tab={tab} />}
      </motion.div>
    </div>
  );
}

function PanelTab({ tab }: { tab: Tab }) {
  const [data, setData] = useState<Panel | null>(null);
  const [err, setErr] = useState("");
  const { ask, node } = useDialog();
  const load = () =>
    api<Panel>("GET", "/api/panel")
      .then(setData)
      .catch((e) => setErr(e.message));
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  if (err) return <div className="text-[15px] text-crit">{err}</div>;
  if (!data) return <div className="text-[15px] text-faint">Loading…</div>;
  return (
    <>
      {tab === "accounts" && <Accounts data={data} reload={load} ask={ask} />}
      {tab === "people" && <People data={data} reload={load} ask={ask} />}
      {tab === "activity" && <Activity data={data} />}
      {node}
    </>
  );
}

// run does a thing, reloads, and surfaces any refusal in a dialog.
async function run(fn: () => Promise<unknown>, reload: () => void, ask: Ask) {
  try {
    await fn();
    reload();
  } catch (e: any) {
    await ask({ title: "That didn't work", note: e.message, confirm: "Close", cancel: "" });
  }
}

// showInvite presents an invite link with a copy button and its expiry.
async function showInvite(ask: Ask, name: string, url: string, expiresAt?: string) {
  await ask({
    title: `Invite for ${name}`,
    fields: [{ name: "link", label: "Send them this link", copy: url }],
    note: `Works once, ${untilExpiry(expiresAt)}. They open it, join in a click, and whatever you share shows up on their machine.`,
    confirm: "Done",
    cancel: "",
  });
}

function Accounts({ data, reload, ask }: { data: Panel; reload: () => void; ask: Ask }) {
  const ready = data.accounts.filter((a) => a.hasLogin).length;
  const need = data.accounts.length - ready;
  const sub = data.accounts.length ? `${ready} ready to share` + (need ? `, ${need} need a login` : "") : "";

  // giveAccess shares an account with people through the gateway. Many people
  // can use one account at once, and access ends the instant it is taken back.
  const give = async (a: Account) => {
    const without = data.people.filter((p) => !(a.shared || []).some((s) => s.personId === p.id));
    if (!data.people.length) return void ask({ title: "Nobody to give it to", note: "Invite someone on the People tab first.", confirm: "Close", cancel: "" });
    if (!without.length) return void ask({ title: "Everyone already has it", note: "Everyone you've added can already use this account.", confirm: "Close", cancel: "" });
    const out = await ask({
      title: `Give access to ${a.name}`,
      fields: [{ name: "people", label: "To", checks: without.map((p) => ({ value: p.id, label: p.name })) }],
      note: "Pick everyone who should use this account. Many people can share it at once, through the gateway — it appears on each machine once they've joined.",
      confirm: "Give access",
    });
    const ids = (out?.people as string[]) || [];
    if (!ids.length) return;
    try {
      let lastGateway = "";
      for (const id of ids) {
        const { gateway } = await api<{ gateway?: string }>("POST", `/api/accounts/${a.id}/share`, { personId: id });
        lastGateway = gateway || lastGateway;
      }
      reload();
      const names = without.filter((p) => ids.includes(p.id)).map((p) => p.name).join(", ");
      await ask(
        lastGateway
          ? { title: "Done", note: `${names} can now use ${a.name}. It appears on each machine within a minute once they've joined.`, confirm: "Close", cancel: "" }
          : { title: "Gateway not set", note: "There's no gateway configured (CLAWDH_GATEWAY_URL), so there's nowhere to point their access yet.", confirm: "Close", cancel: "" }
      );
    } catch (e: any) {
      await ask({ title: "That didn't work", note: e.message, confirm: "Close", cancel: "" });
    }
  };

  const revoke = async (a: Account, sh: { shareId: string; personName: string }) => {
    const ok = await ask({ title: `Take ${a.name} away from ${sh.personName}?`, note: "Their access stops within seconds, and any session they're running on it ends.", confirm: "Take it away", danger: true });
    if (ok) run(() => api("POST", `/api/shares/${sh.shareId}/revoke`), reload, ask);
  };

  // howToAddLogin explains the one thing the panel can't do itself: sign in.
  const howToAddLogin = (a: Account) =>
    ask({
      title: `Add a login for ${a.name}`,
      note: "The panel can't sign in for you — Claude's login needs a real browser. On the machine where this account is signed in, open the clawdh page there and choose “Add to panel” on it. That hands its login up here. People you share it with never do this.",
      confirm: "Got it",
      cancel: "",
    });

  const removeAccount = async (a: Account) => {
    const ok = await ask({ title: `Remove ${a.name}?`, note: (a.shared || []).length ? "Everyone using it loses access." : "", confirm: "Remove", danger: true });
    if (ok) run(() => api("DELETE", `/api/accounts/${a.id}`), reload, ask);
  };

  const addAccount = async () => {
    const out = await ask({
      title: "Add an account",
      fields: [
        { name: "name", label: "Name", placeholder: "Work" },
        { name: "email", label: "Its Claude sign-in (optional)", placeholder: "work@example.com" },
      ],
      note: "This makes an empty slot. To share it, add its login from the machine where it's signed in — see “How to add its login”.",
      confirm: "Add",
    });
    if (out?.name) run(() => api("POST", "/api/accounts", out), reload, ask);
  };

  return (
    <div>
      <Head
        title="Accounts"
        sub={sub || "The Claude logins your team shares through the gateway."}
        action={<PrimaryButton onClick={addAccount}>Add an account</PrimaryButton>}
      />
      {data.accounts.length === 0 ? (
        <Empty>No accounts yet. On a machine where a Claude account is signed in, open its clawdh page and choose “Add to panel” — it shows up here, ready to share.</Empty>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2">
          {data.accounts.map((a) => (
            <motion.div key={a.id} whileHover={{ y: -2 }} transition={{ duration: 0.15 }} className="flex flex-col rounded-2xl border border-line bg-raised p-5 transition-colors hover:border-line/0 hover:ring-1 hover:ring-primary/25">
              <div className="flex items-start gap-2">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="truncate text-[17px] font-semibold">{a.name}</span>
                    {a.warning && <span title={a.warning} className="cursor-help text-warn">⚠</span>}
                  </div>
                  {/* The second line is what the name doesn't already say: the email
                      (unless the account is named by it), else the plan. */}
                  <div className="mt-0.5 text-[14px] text-faint">
                    {a.email && a.email.toLowerCase() !== a.name.toLowerCase() ? a.email : a.plan ? a.plan + " plan" : a.email ? "" : "no sign-in on file"}
                  </div>
                </div>
                {a.hasLogin ? <Pill kind="ok">Ready</Pill> : <Pill kind="need">Needs a login</Pill>}
              </div>
              {a.warning && (
                <div className="mt-3 rounded-lg border border-warn/30 bg-warn/10 px-3 py-2 text-[13px] leading-snug text-warn">{a.warning}</div>
              )}

              <div className="mt-4 min-h-[28px]">
                {(a.shared || []).length ? (
                  <div className="flex flex-wrap gap-1.5">
                    {(a.shared || []).map((sh) => (
                      <span key={sh.shareId} className="group inline-flex items-center gap-1.5 rounded-lg border border-line bg-raised-2 py-1 pl-2.5 pr-1.5 text-[13.5px]">
                        {sh.personName}
                        <button onClick={() => revoke(a, sh)} className="rounded text-faint transition-colors hover:text-crit" title="Take access away">×</button>
                      </span>
                    ))}
                  </div>
                ) : a.hasLogin ? (
                  <div className="text-[14px] text-faint">Not shared with anyone yet.</div>
                ) : (
                  <div className="text-[14px] text-faint">Add its login on the machine where it's signed in.</div>
                )}
              </div>

              <div className="mt-4 flex items-center gap-2 border-t border-line pt-3">
                {a.hasLogin ? (
                  <button onClick={() => give(a)} className="rounded-lg bg-primary/12 px-3.5 py-2 text-[14px] font-semibold text-primary transition-colors hover:bg-primary/20">Give access</button>
                ) : (
                  <button onClick={() => howToAddLogin(a)} className="rounded-lg bg-primary/12 px-3.5 py-2 text-[14px] font-semibold text-primary transition-colors hover:bg-primary/20">How to add its login</button>
                )}
                <button onClick={() => removeAccount(a)} className="ml-auto rounded-lg px-2.5 py-2 text-[14px] text-faint transition-colors hover:text-crit">Remove</button>
              </div>
            </motion.div>
          ))}
        </div>
      )}
    </div>
  );
}

function People({ data, reload, ask }: { data: Panel; reload: () => void; ask: Ask }) {
  const setUp = data.people.filter((p) => (p.devices || []).length).length;
  const sub = data.people.length ? `${data.people.length} ${data.people.length === 1 ? "person" : "people"}, ${setUp} set up` : "";
  const [jobsFor, setJobsFor] = useState<Device | null>(null);

  // inviteSomeone adds a person and hands you their invite link in one step.
  const inviteSomeone = async () => {
    const out = await ask({
      title: "Invite someone",
      fields: [
        { name: "name", label: "Their name", placeholder: "Ehtisham" },
        { name: "email", label: "Email (optional)" },
      ],
      confirm: "Create invite",
    });
    if (!out?.name) return;
    try {
      await api("POST", "/api/people", { name: out.name, email: out.email });
      const target = (await api<Panel>("GET", "/api/panel")).people.find((p) => p.name === out.name);
      reload();
      if (!target) return;
      const { url, expiresAt } = await api<{ url: string; expiresAt?: string }>("POST", `/api/people/${target.id}/invite`);
      await showInvite(ask, String(out.name), url, expiresAt);
    } catch (e: any) {
      await ask({ title: "That didn't work", note: e.message, confirm: "Close", cancel: "" });
    }
  };

  // invitePerson makes a fresh invite link for someone already added.
  const invitePerson = async (p: Person) => {
    try {
      const { url, expiresAt } = await api<{ url: string; expiresAt?: string }>("POST", `/api/people/${p.id}/invite`);
      await showInvite(ask, p.name, url, expiresAt);
    } catch (e: any) {
      await ask({ title: "That didn't work", note: e.message, confirm: "Close", cancel: "" });
    }
  };

  const removePerson = async (p: Person) => {
    const ok = await ask({ title: `Remove ${p.name}?`, note: "Their access ends and their machines stop working straight away.", confirm: "Remove", danger: true });
    if (ok) run(() => api("DELETE", `/api/people/${p.id}`), reload, ask);
  };

  const cutOff = async (p: Person, d: Device) => {
    const ok = await ask({ title: `Remove ${d.name}?`, note: `${p.name}'s other machines keep working.`, confirm: "Remove", danger: true });
    if (ok) run(() => api("DELETE", `/api/devices/${d.id}`), reload, ask);
  };

  return (
    <div>
      <Head
        title="People"
        sub={sub || "Everyone on the team, the accounts they can use, and the machines they're on."}
        action={<PrimaryButton onClick={inviteSomeone}>Invite someone</PrimaryButton>}
      />
      {data.people.length === 0 ? (
        <Empty>Nobody yet. Invite someone — they get a link, join in a click, and whatever you share appears on their machine.</Empty>
      ) : (
        <div className="flex flex-col gap-3">
          {data.people.map((p: Person) => (
            <motion.div key={p.id} whileHover={{ y: -2 }} transition={{ duration: 0.15 }} className="rounded-2xl border border-line bg-raised p-5 transition-colors hover:ring-1 hover:ring-primary/25">
              <div className="flex items-start gap-3">
                <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-primary/15 text-[16px] font-bold text-primary">
                  {p.name.slice(0, 1).toUpperCase()}
                </div>
                <div className="min-w-0 flex-1">
                  <div className="text-[17px] font-semibold">{p.name}</div>
                  <div className="text-[14px] text-faint">{p.email || "no email"}</div>
                </div>
                <div className="flex shrink-0 items-center gap-1">
                  <button onClick={() => invitePerson(p)} className="rounded-lg px-3 py-1.5 text-[14px] font-medium text-primary transition-colors hover:bg-primary/12">Invite link</button>
                  <button onClick={() => removePerson(p)} className="rounded-lg px-2.5 py-1.5 text-[14px] text-faint transition-colors hover:text-crit">Remove</button>
                </div>
              </div>

              <div className="mt-4 grid gap-4 sm:grid-cols-2">
                <div>
                  <div className="text-[12px] font-medium uppercase tracking-[0.06em] text-faint">Can use</div>
                  <div className="mt-1.5 flex flex-wrap gap-1.5">
                    {(p.can || []).length ? (
                      (p.can || []).map((c) => (
                        <span key={c} className="rounded-lg border border-line bg-raised-2 px-2.5 py-1 text-[13.5px]">{c}</span>
                      ))
                    ) : (
                      <span className="text-[14px] text-faint">Nothing shared yet — share an account from the Accounts tab.</span>
                    )}
                  </div>
                </div>
                <div>
                  <div className="text-[12px] font-medium uppercase tracking-[0.06em] text-faint">Machines</div>
                  <div className="mt-1.5">
                    {(p.devices || []).length ? (
                      (p.devices || []).map((d) => (
                        <div key={d.id} className="flex items-center gap-2 py-1 text-[13.5px]">
                          <span className="font-mono text-ink">{d.name}</span>
                          {d.remote && <span title="Remote help is on" className="rounded-full bg-primary/12 px-1.5 py-px text-[11px] font-medium text-primary">remote</span>}
                          <span className="text-faint">{when(d.lastSeen)}</span>
                          <div className="ml-auto flex gap-1">
                            {d.remote && <button onClick={() => setJobsFor(d)} className="rounded px-1.5 py-0.5 text-[13px] text-primary transition-colors hover:bg-primary/12">Remote help</button>}
                            <button onClick={() => cutOff(p, d)} title="Unlink this machine" className="rounded px-1.5 py-0.5 text-[13px] text-faint transition-colors hover:text-crit">Forget</button>
                          </div>
                        </div>
                      ))
                    ) : (
                      <span className="text-[14px] text-faint">Not joined yet — send them an invite link.</span>
                    )}
                  </div>
                </div>
              </div>
            </motion.div>
          ))}
        </div>
      )}
      <AnimatePresence>
        {jobsFor && <DeviceJobs key={jobsFor.id} deviceId={jobsFor.id} deviceName={jobsFor.name} onClose={() => setJobsFor(null)} />}
      </AnimatePresence>
    </div>
  );
}

function Activity({ data }: { data: Panel }) {
  const events = data.activity || [];
  return (
    <div>
      <Head title="Activity" sub="Everything that's happened — shares given and taken, people invited, machines joined." />
      {events.length === 0 ? (
        <Empty>Nothing yet. Actions you take here show up as a running log.</Empty>
      ) : (
        <div className="relative ml-2 border-l border-line pl-6">
          {events.map((e, i) => (
            <div key={i} className="relative pb-6 last:pb-0">
              <span className="absolute -left-[27px] top-1.5 h-2.5 w-2.5 rounded-full border-2 border-ground bg-primary" />
              <div className="text-[15px] leading-snug">
                <b className="font-semibold">{e.who}</b> <span className="text-muted">{e.what}</span>
              </div>
              <time className="text-[13px] text-faint">{when(e.at) || new Date(e.at).toLocaleString()}</time>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function Head({ title, sub, action }: { title: string; sub: string; action?: React.ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-4">
      <div>
        <h1 className="text-[28px] font-bold tracking-tight">{title}</h1>
        <div className="mt-1 max-w-xl text-[15px] leading-relaxed text-muted">{sub}</div>
      </div>
      {action}
    </div>
  );
}

function PrimaryButton({ onClick, children }: { onClick: () => void; children: React.ReactNode }) {
  return (
    <motion.button
      onClick={onClick}
      whileTap={{ scale: 0.97 }}
      className="rounded-xl bg-primary px-5 py-2.5 text-[15px] font-semibold text-sunken shadow-lg shadow-primary/20 transition-[filter] hover:brightness-110"
    >
      {children}
    </motion.button>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="rounded-2xl border border-dashed border-line px-7 py-12 text-center text-[15px] leading-relaxed text-muted">{children}</div>;
}

function Pill({ kind, children }: { kind: "ok" | "need"; children: React.ReactNode }) {
  const on = kind === "ok";
  return (
    <span className={`inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-1 text-[13px] font-medium ${on ? "border-ok/30 bg-ok/10 text-ok" : "border-warn/30 bg-warn/10 text-warn"}`}>
      <span className={`h-[6px] w-[6px] rounded-full ${on ? "bg-ok" : "bg-warn"}`} />
      {children}
    </span>
  );
}
