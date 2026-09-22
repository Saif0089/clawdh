// clawdh web UI — plain JS, no build step. Small enough that a framework
// would cost more than it saves.
"use strict";

const accountsList = document.getElementById("accounts-list");
const accountsEmpty = document.getElementById("accounts-empty");
const rowTemplate = document.getElementById("account-row-template");
const meterTemplate = document.getElementById("meter-template");
const refreshedLabel = document.getElementById("refreshed");
const buildTag = document.getElementById("build-tag");

const connectBlock = document.getElementById("connect-block");
const connectedNote = document.getElementById("connected-note");

// How often the page re-reads everything. The numbers here are the reason
// it is open, so it keeps them current itself rather than making someone
// press a button and wonder whether they had to.
const POLL_MS = 5000;

// Every account runs through `clawdh` — no shell aliases to install, keep in
// sync, or get wrong on Windows. One command shape, the same everywhere.

async function api(path, opts) {
  const res = await fetch(path, opts);
  if (!res.ok) {
    let message = res.statusText;
    try {
      const body = await res.json();
      if (body && body.error) message = body.error;
    } catch (_) {}
    throw new Error(message);
  }
  const text = await res.text();
  if (!text) return null;
  return JSON.parse(text);
}

// --- run commands -----------------------------------------------------

// runCommand is what a person types to start an account, shown per OS so it
// always matches what their shell actually has.
function runCommand(account) {
  if (account.kind === "default") return "claude";
  return `clawdh ${account.slug}`;
}
function sharedRunCommand(slug) {
  return `clawdh shared ${slug}`;
}

// --- time formatting --------------------------------------------------

function formatLeft(ms) {
  if (!(ms > 0)) return "now";
  const minutes = Math.floor(ms / 60000);
  if (minutes < 1) return "under a minute";
  const days = Math.floor(minutes / 1440);
  const hours = Math.floor((minutes % 1440) / 60);
  const mins = minutes % 60;
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
  return `${mins}m`;
}
// Every clock time on this page is 12-hour with am/pm, whatever the browser's
// locale would otherwise pick. A machine set to a 24-hour locale used to show
// "15:52" here and "3:52 pm" in the menu bar beside it; one of the two had to
// win, and the one people read the rest of their day in is this one.
function clockTime(date) {
  return date.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit", hour12: true }).toLowerCase();
}
// The same instant with its date, for anything that may not be today.
function dateAndTime(date) {
  return `${fullDate(date)}, ${clockTime(date)}`;
}
function formatWhen(date) {
  const time = clockTime(date);
  const midnight = new Date();
  midnight.setHours(0, 0, 0, 0);
  const dayIndex = Math.floor((date - midnight) / 86400000);
  if (dayIndex === 0) return `today ${time}`;
  if (dayIndex === 1) return `tomorrow ${time}`;
  if (dayIndex > 1 && dayIndex < 7) return `${date.toLocaleDateString("en-US", { weekday: "long" })} ${time}`;
  return fullDate(date);
}
function formatAgo(ms) {
  const minutes = Math.floor(ms / 60000);
  if (minutes < 1) return "less than a minute ago";
  const hours = Math.floor(minutes / 60);
  const mins = minutes % 60;
  if (hours === 0) return `${mins} min ago`;
  return mins > 0 ? `${hours} h ${mins} min ago` : `${hours} h ago`;
}
function fullDate(date) { return date.toLocaleDateString("en-US", { month: "short", day: "numeric" }); }

// Countdowns tick in the browser from the absolute timestamps the server
// sent, so the page stays honest while it sits open without polling.
const countdowns = [];
function countdown(el, isoTime, render) {
  const at = new Date(isoTime);
  if (isNaN(at)) return false;
  const entry = { el, at, render };
  countdowns.push(entry);
  tickOne(entry);
  return true;
}
function tickOne(entry) { entry.el.textContent = entry.render(entry.at - Date.now(), entry.at); }
setInterval(() => {
  for (let i = countdowns.length - 1; i >= 0; i--) {
    if (!countdowns[i].el.isConnected) countdowns.splice(i, 1);
    else tickOne(countdowns[i]);
  }
  accountsList.querySelectorAll(".card").forEach(tickAge);
}, 1000);

function planLabel(plan) {
  if (!plan) return "";
  const known = { max: "Max", pro: "Pro", team: "Team", enterprise: "Enterprise", free: "Free" };
  const key = plan.toLowerCase();
  return (known[key] || plan.charAt(0).toUpperCase() + plan.slice(1)) + " plan";
}
function levelFor(percent, severity) {
  if (severity === "critical" || severity === "error" || percent >= 90) return "level-crit";
  if (severity === "warning" || severity === "warn" || percent >= 70) return "level-warn";
  return "level-ok";
}

// --- loading & rendering ---------------------------------------------

// logins from discovery, keyed by configDir, so an account card can show the
// email its login belongs to and know whether there's a login to share.
let loginsByDir = {};

async function loadLogins() {
  try {
    const data = await api("/api/logins");
    const map = {};
    for (const l of (data.logins || [])) map[l.configDir || ""] = l;
    loginsByDir = map;
  } catch (_) {
    // Discovery is an enhancement (email, shareability); its absence must not
    // stop the accounts themselves rendering.
  }
}

// currentAccounts is the last list the server gave, kept so the sessions block
// can offer the same accounts the cards below show — one page, one answer to
// "which accounts are there".
let currentAccounts = [];

let accountsGeneration = 0;
async function loadAccounts() {
  const generation = accountsGeneration;
  const data = await api("/api/accounts");
  if (generation !== accountsGeneration) return;
  renderAccounts(data.accounts || []);
}
function refreshAccounts() {
  accountsGeneration++;
  Promise.all([loadLogins(), loadAccounts()]).catch((err) => {
    refreshedLabel.textContent = "could not refresh: " + err.message;
  });
}

// The list is rebuilt only when one of these moves, so a poll every few
// seconds doesn't throw away a "Copied" button or restart a countdown.
function listSignature(accounts) {
  return JSON.stringify([
    accounts.map((a) => {
      const l = loginsByDir[a.configDir || ""] || {};
      return [a.id, a.name, a.alias, a.slug, a.status, a.kind, l.email || ""];
    }),
    // The shares belong in here too, now that they decide which cards exist
    // and what each one is called. Left out, the list had to be rebuilt on
    // every poll to pick them up — which threw away an open menu, a half-made
    // choice and a "Copied" button every few seconds.
    currentShares.map((sh) => [sh.slug, sh.account, shareEmail(sh), !!sh.contributed]),
  ]);
}
let renderedSignature = null;

// mergedAccounts is every account this machine can run, once each.
//
// A login you lend to the panel exists twice as far as the API is concerned —
// as a local account with a config directory, and as a share of that same
// login coming back down — and the page used to draw both, side by side, with
// identical bars read from the identical gateway reading. They are one account
// and they are paired the way the server pairs them: by the login's email.
function mergedAccounts(accounts, shares) {
  const out = [];
  const claimed = new Set();
  for (const account of accounts) {
    const login = loginsByDir[account.configDir || ""];
    const email = login && login.email ? login.email.toLowerCase() : "";
    const share = email ? shares.find((sh) => shareEmail(sh) === email) : null;
    if (share) claimed.add(share.slug);
    out.push({ account, share, email: (login && login.email) || "", name: account.name });
  }
  // A share of somebody else's login has no local account behind it.
  for (const share of shares) {
    if (claimed.has(share.slug)) continue;
    out.push({ account: null, share, email: shareEmail(share), name: share.account });
  }
  return out;
}

function shareEmail(share) {
  const e = (share.window && share.window.email) || share.email || "";
  return e.toLowerCase();
}

function renderAccounts(accounts) {
  currentAccounts = accounts;
  const rows = mergedAccounts(accounts, currentShares);
  const signature = listSignature(accounts);
  if (signature === renderedSignature && accountsList.children.length === rows.length) {
    refreshVisibleUsage(rows);
    return;
  }
  renderedSignature = signature;
  accountsList.innerHTML = "";
  accountsEmpty.hidden = rows.length > 0;

  for (const row of rows) {
    accountsList.appendChild(buildAccountCard(row));
  }
}

function buildAccountCard(row) {
  const { account, share } = row;
  const node = rowTemplate.content.cloneNode(true);
  const card = node.querySelector(".card");
  card.dataset.id = account ? account.id : "share:" + share.slug;
  if (share) card.dataset.slug = share.slug;

  node.querySelector(".account-name").textContent = row.name;
  node.querySelector(".account-email").textContent = row.email;
  node.querySelector(".shared-chip").hidden = !share;

  // A login the gateway holds runs through the gateway; the copy still on this
  // machine is dead by design, so `clawdh <name>` would only fail.
  const cmd = share ? sharedRunCommand(share.slug) : runCommand(account);
  node.querySelector(".run-cmd").textContent = cmd;
  const copyBtn = node.querySelector(".copy-cmd");
  copyBtn.addEventListener("click", () => copyToClipboard(cmd, copyBtn));

  buildCardMenu(node, row);

  if (account) {
    setStatus(node.querySelector(".status-pill"), account.status);
    refreshUsage(account, card);
  } else {
    setStatus(node.querySelector(".status-pill"), "linked");
    renderShareWindow(card, share);
  }
  renderPeople(card.querySelector(".people"), share && share.window ? share.window.people : null);
  return node;
}

// buildCardMenu fills the kebab with the things that apply to this account,
// and hides the button outright when none of them do — an empty menu is worse
// than no menu.
function buildCardMenu(node, row) {
  const { account, share } = row;
  const menu = node.querySelector(".kebab");
  const connectBtn = node.querySelector(".connect-btn");
  const shareBtn = node.querySelector(".share-btn");
  const withdrawBtn = node.querySelector(".withdraw-btn");
  const renameBtn = node.querySelector(".rename-btn");
  const removeBtn = node.querySelector(".remove-btn");

  if (!account) {
    // Somebody else's login. The only thing that could be yours to do is take
    // it back, and only if you were the one who put it there.
    connectBtn.remove();
    shareBtn.remove();
    renameBtn.remove();
    removeBtn.remove();
    if (share.contributed && share.accountId) {
      withdrawBtn.hidden = false;
      withdrawBtn.addEventListener("click", () => withdrawFromPanel(share));
    } else {
      menu.remove();
      return;
    }
    wireMenu(menu);
    return;
  }

  const login = loginsByDir[account.configDir || ""];
  const isDefault = account.kind === "default";

  connectBtn.textContent = account.status === "linked" ? "Reconnect" : "Connect";
  connectBtn.addEventListener("click", () => startLogin(account));

  if (login) {
    shareBtn.textContent = share ? "Refresh on the panel" : "Add to panel";
    if (share) shareBtn.title = "Hands up a fresh login, which is how a broken one is mended.";
    shareBtn.addEventListener("click", () => openShareDialog(account, login, share));
  } else {
    shareBtn.disabled = true;
    shareBtn.title = "Sign this account in first, then it can be shared.";
  }

  if (share && share.contributed && share.accountId) {
    withdrawBtn.hidden = false;
    withdrawBtn.addEventListener("click", () => withdrawFromPanel(share));
  }

  renameBtn.addEventListener("click", () => {
    document.getElementById("rename-id").value = account.id;
    document.getElementById("rename-name").value = account.name;
    renameDialog.showModal();
  });

  removeBtn.textContent = isDefault ? "Forget" : "Remove";
  removeBtn.addEventListener("click", () => removeAccount(account, isDefault));
  wireMenu(menu);
}

// One menu open at a time, closing on a click away or Escape — the behaviour
// the shape already promises.
function wireMenu(menu) {
  const btn = menu.querySelector(".kebab-btn");
  const pop = menu.querySelector(".kebab-pop");
  const close = () => { pop.hidden = true; btn.setAttribute("aria-expanded", "false"); };
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    const opening = pop.hidden;
    document.querySelectorAll(".kebab-pop").forEach((p) => { p.hidden = true; });
    document.querySelectorAll(".kebab-btn").forEach((b) => b.setAttribute("aria-expanded", "false"));
    if (opening) { pop.hidden = false; btn.setAttribute("aria-expanded", "true"); }
  });
  pop.addEventListener("click", close);
}
document.addEventListener("click", () => {
  document.querySelectorAll(".kebab-pop").forEach((p) => { p.hidden = true; });
  document.querySelectorAll(".kebab-btn").forEach((b) => b.setAttribute("aria-expanded", "false"));
});
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  document.querySelectorAll(".kebab-pop").forEach((p) => { p.hidden = true; });
});

// renderShareWindow draws the gateway's reading for an account with no local
// login behind it — the same bars a local card gets, from the same numbers.
function renderShareWindow(card, share) {
  const meters = card.querySelector(".meters");
  const w = share.window;
  if (!meters || !w) return;
  meters.replaceChildren();
  const limits = [
    { label: "Current session", percent: (w.fiveH || 0) * 100, resetsAt: w.fiveHReset },
    { label: "This week, all models", percent: (w.sevenD || 0) * 100, resetsAt: w.sevenDReset },
  ];
  for (const m of (w.models || [])) limits.push({ label: m.label, percent: m.percent || 0, resetsAt: m.resetsAt });
  for (const limit of limits) meters.appendChild(buildMeter(limit, false));
}

function refreshVisibleUsage(rows) {
  for (const row of rows) {
    const id = row.account ? row.account.id : "share:" + row.share.slug;
    const card = accountsList.querySelector(`.card[data-id="${CSS.escape(id)}"]`);
    if (!card) continue;
    if (row.account) refreshUsage(row.account, card);
    else renderShareWindow(card, row.share);
    renderPeople(card.querySelector(".people"), row.share && row.share.window ? row.share.window.people : null);
  }
}

// --- status ------------------------------------------------------------

// Each state maps to a word and a colour class. The words say whether the
// account works right now, not how it is stored.
const STATUS = {
  linked: { text: "Ready", cls: "ok" },
  unknown: { text: "Ready", cls: "ok" },
  pending: { text: "Not signed in", cls: "" },
  expired: { text: "Sign-in expired", cls: "crit" },
  "signed-out": { text: "Signed out", cls: "crit" },
};
function setStatus(pill, status) {
  const s = STATUS[status] || { text: status, cls: "" };
  pill.className = "status-pill " + s.cls;
  pill.textContent = s.text;
}

// --- usage ------------------------------------------------------------

const usageInFlight = new WeakSet();
function refreshUsage(account, card) {
  if (usageInFlight.has(card)) return;
  usageInFlight.add(card);
  loadUsage(account, card).finally(() => usageInFlight.delete(card));
}
async function loadUsage(account, card) {
  let snapshot;
  try {
    snapshot = await api(`/api/accounts/${account.id}/usage`);
  } catch (err) {
    snapshot = { error: "Could not read usage: " + err.message };
  }
  if (!card.isConnected) return;
  renderUsage(card, account, snapshot);
}

function usageSignature(account, snapshot) {
  const limits = ((snapshot.usage && snapshot.usage.limits) || []).map((l) => [l.label, l.percent, l.severity, l.resetsAt || ""]);
  const session = snapshot.session || {};
  return JSON.stringify([liveStatus(account, snapshot), session.plan || "", session.accessExpiresAt || "", session.sessionExpiresAt || "", limits]);
}
const drawnFrom = new WeakMap();

function renderUsage(card, account, snapshot) {
  noteReadAt(card, snapshot);
  showNote(card, snapshot.error || snapshot.note);
  const signature = usageSignature(account, snapshot);
  if (drawnFrom.get(card) === signature) return;
  const firstPaint = !drawnFrom.has(card);
  drawnFrom.set(card, signature);

  const meters = card.querySelector(".meters");
  meters.innerHTML = "";

  const state = liveStatus(account, snapshot);
  setStatus(card.querySelector(".status-pill"), state);
  card.querySelector(".plan-chip").textContent = planLabel(snapshot.session && snapshot.session.plan);

  for (const limit of (snapshot.usage && snapshot.usage.limits) || []) {
    meters.appendChild(buildMeter(limit, firstPaint));
  }
  showSessionLine(card, state);
}

function showNote(card, text) {
  const note = card.querySelector(".usage-note");
  note.hidden = !text;
  if (text && note.textContent !== text) note.textContent = text;
}

const STALE_AFTER_MS = 2 * 60_000;
const numbersReadAt = new WeakMap();
function noteReadAt(card, snapshot) {
  const at = new Date(snapshot.usage ? snapshot.usage.fetchedAt : NaN);
  if (isNaN(at)) numbersReadAt.delete(card);
  else numbersReadAt.set(card, at);
  tickAge(card);
}
function tickAge(card) {
  const label = card.querySelector(".usage-age");
  if (!label) return;
  const at = numbersReadAt.get(card);
  const age = at ? Date.now() - at : 0;
  label.hidden = !(age > STALE_AFTER_MS);
  if (label.hidden) return;
  const midnight = new Date();
  midnight.setHours(0, 0, 0, 0);
  const time = clockTime(at);
  const when = at >= midnight ? time : dateAndTime(at);
  label.textContent = `as of ${when} · ${formatAgo(age)}`;
}

function liveStatus(account, snapshot) {
  return snapshot.state || (account.status === "linked" ? "unknown" : account.status);
}

function buildMeter(limit, animate) {
  const node = meterTemplate.content.cloneNode(true);
  const meter = node.querySelector(".meter");
  const percent = Math.max(0, Math.min(100, limit.percent || 0));
  meter.classList.add(levelFor(percent, limit.severity));
  node.querySelector(".meter-label").textContent = limit.label;
  node.querySelector(".meter-pct").textContent = Math.round(percent) + "%";
  const bar = node.querySelector(".bar");
  bar.setAttribute("aria-valuenow", Math.round(percent));
  bar.setAttribute("aria-label", limit.label);
  // Set straight away rather than inside a frame callback. The width IS the
  // reading, and a bar whose fill waits on rAF is a bar that renders empty
  // wherever that callback is throttled or never runs. The CSS transition
  // still animates every later change, which is the one worth seeing.
  const fill = node.querySelector(".bar-fill");
  fill.style.width = percent + "%";
  const reset = node.querySelector(".meter-reset");
  if (limit.resetsAt) {
    // On the line, not under it: how long is left is the useful half, and the
    // exact moment is one hover away.
    countdown(reset, limit.resetsAt, (ms, at) => {
      reset.title = ms > 0 ? "Resets " + formatWhen(at) : "";
      return ms > 0 ? formatLeft(ms) + " left" : "resetting";
    });
  } else {
    reset.remove();
  }
  return node;
}

// showSessionLine replaces the old pair of easily-confused clocks (a long
// "login session" one and a short "access token" one) with a single line that
// only appears when the sign-in needs attention. When it's fine, the Ready
// pill already said so, and a ticking countdown was only ever a distraction.
function showSessionLine(card, state) {
  const line = card.querySelector(".session-line");
  if (state === "expired" || state === "signed-out") {
    line.hidden = false;
    line.className = "session-line crit";
    line.textContent = "Reconnect to use this account again.";
  } else {
    line.hidden = true;
  }
}

async function copyToClipboard(text, button) {
  const original = button.textContent;
  try { await navigator.clipboard.writeText(text); button.textContent = "Copied"; }
  catch (_) { button.textContent = "Copy failed"; }
  setTimeout(() => { button.textContent = original; }, 1500);
}

async function removeAccount(account, isDefault) {
  const question = isDefault
    ? `Stop showing "${account.name}" here? Your main ~/.claude login is left untouched — clawdh just forgets it.`
    : `Remove "${account.name}"? This deletes its local login on this machine.`;
  if (!confirm(question)) return;
  try { await api(`/api/accounts/${account.id}`, { method: "DELETE" }); }
  catch (err) { alert("Could not remove account: " + err.message); }
  refreshAccounts();
}

// --- shared accounts ---------------------------------------------------

// currentShares is what the last panel refresh said this machine may run, kept
// so an account card can tell whether its login is already up there.
let currentShares = [];

// renderShared takes the shares the panel last sent and redraws the one
// account list they belong in. There is no separate shared section any more:
// a share either pairs with a login on this machine or stands as its own card,
// and both are decided in mergedAccounts.
function renderShared(shares) {
  currentShares = shares || [];
  renderAccounts(currentAccounts);
}

// --- who spent the week ------------------------------------------------

// A shared account is a shared cost, so the machine using it shows who has
// been spending it — not only the panel's admin. The number is each person's
// slice of the real weekly allowance, the same unit as the week's own bar
// above it, so 100% can only ever mean the plan is gone.

// Eight hues, picked by a stable hash of the name, so a colleague keeps the
// same colour on every card and every machine. Colour identifies a person
// here; it never carries how full anything is — the meters above do that.
const PERSON_HUES = ["#6366F1", "#22C55E", "#F59E0B", "#EC4899", "#06B6D4", "#A855F7", "#14B8A6", "#F97316"];
function personColor(name) {
  let h = 5381;
  for (let i = 0; i < name.length; i++) h = ((h << 5) + h + name.charCodeAt(i)) >>> 0;
  return PERSON_HUES[h % PERSON_HUES.length];
}

function renderPeople(box, people) {
  if (!box) return;
  box.replaceChildren();
  if (!people || !people.length) return;
  for (const p of people) {
    const row = document.createElement("div");
    row.className = "person";

    const dot = document.createElement("span");
    dot.className = "person-dot";
    dot.style.background = personColor(p.name || "");

    const name = document.createElement("span");
    name.className = "person-name";
    name.textContent = p.name || "Someone";

    const bar = document.createElement("span");
    bar.className = "person-bar";
    const fill = document.createElement("span");
    fill.className = "person-fill";
    fill.style.width = Math.min(1, Math.max(0, p.ofWeekly || 0)) * 100 + "%";
    fill.style.background = personColor(p.name || "");
    bar.appendChild(fill);

    const pct = document.createElement("span");
    pct.className = "person-pct";
    pct.textContent = formatPercent((p.ofWeekly || 0) * 100);

    row.append(dot, name, bar, pct);
    row.title = `${p.name} used ${pct.textContent} of this account's week`;
    box.appendChild(row);
  }
}

function formatPercent(p) {
  if (p > 0 && p < 1) return p.toFixed(1) + "%";
  return Math.round(p) + "%";
}

// --- join / connected --------------------------------------------------

async function refreshPanel() {
  let st;
  try { st = await api("/api/panel"); }
  catch { return; }
  renderShared(st.shared);
  if (st.enrolled) {
    connectBlock.hidden = true;
    connectedNote.hidden = false;
    connectedNote.replaceChildren();
    const who = st.personName ? ` — you are ` : "";
    const line = document.createElement("span");
    line.append(document.createTextNode("Connected to "));
    const host = document.createElement("b");
    host.textContent = prettyURL(st.server);
    line.append(host);
    if (st.personName) {
      line.append(document.createTextNode(who));
      const b = document.createElement("b");
      b.textContent = st.personName;
      line.append(b);
    }
    const spacer = document.createElement("span");
    spacer.className = "spacer";
    const dc = document.createElement("button");
    dc.textContent = "Disconnect";
    dc.addEventListener("click", disconnectFromPanel);
    connectedNote.append(line, spacer, dc);
  } else {
    connectedNote.hidden = true;
    connectBlock.hidden = false;
  }
}

async function connectWithInvite() {
  await connectFrom(document.getElementById("invite-input").value.trim());
}

// connectFrom joins the panel an invite link names — from the box, or from the
// invite the page was opened with. It reports into the box's error line either
// way, which is where a person looks when nothing appears.
async function connectFrom(raw) {
  const err = document.getElementById("connect-err");
  err.textContent = "";
  const parsed = parseInvite(raw);
  if (!parsed) { err.textContent = "Paste the whole invite link you were sent."; return; }
  const btn = document.getElementById("connect-btn");
  btn.disabled = true; btn.textContent = "Connecting…";
  try {
    await api("/api/panel/connect", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ server: parsed.server, code: parsed.code }),
    });
    document.getElementById("invite-input").value = "";
    await refreshPanel();
    refreshAccounts();
  } catch (e) {
    err.textContent = e.message;
  } finally {
    btn.disabled = false; btn.textContent = "Connect";
  }
}

// The invite page's one button opens this page with the invite in the URL —
// so the person never copies the link. Join at once, and take the invite out
// of the address bar so a reload or a bookmark does not try a used code again.
(function joinFromURL() {
  const invite = new URLSearchParams(location.search).get("invite");
  if (!invite) return;
  history.replaceState(null, "", location.pathname);
  connectBlock.hidden = false;
  document.getElementById("invite-input").value = invite;
  connectFrom(invite).then(() => {
    const note = document.getElementById("connected-note");
    if (!note.hidden) note.scrollIntoView({ block: "center" });
  });
})();

// parseInvite pulls the panel URL and code out of a pasted invite link. It also
// accepts a bare "url code" for anyone who has them separately.
function parseInvite(text) {
  if (!text) return null;
  const parts = text.split(/\s+/);
  if (parts.length === 2 && /^https?:\/\//i.test(parts[0])) {
    return { server: parts[0].replace(/\/+$/, ""), code: parts[1] };
  }
  try {
    const u = new URL(text);
    const origin = u.origin;
    const q = u.searchParams.get("code");
    if (q) return { server: origin, code: q };
    if (u.hash) return { server: origin, code: u.hash.slice(1) };
    const segs = u.pathname.split("/").filter(Boolean);
    if (segs.length >= 2 && (segs[segs.length - 2] === "i" || segs[segs.length - 2] === "join")) {
      return { server: origin, code: segs[segs.length - 1] };
    }
  } catch (_) {}
  return null;
}

// withdrawFromPanel takes a login this person added back off the panel.
// Everyone sharing it loses access — that is what taking it back means.
async function withdrawFromPanel(sh) {
  if (!confirm(`Take ${sh.account} back off the panel?\n\nEveryone it is shared with loses access right away. The login stays signed in on this machine.`)) return;
  try {
    await api("/api/logins/withdraw", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ accountId: sh.accountId }),
    });
    await refreshPanel();
    refreshAccounts();
  } catch (e) {
    alert("Could not take it back: " + e.message);
  }
}

async function disconnectFromPanel() {
  if (!confirm("Disconnect this machine? Shared accounts stop appearing here.")) return;
  try { await api("/api/panel/disconnect", { method: "POST" }); await refreshPanel(); }
  catch (e) { alert("Could not disconnect: " + e.message); }
}

function prettyURL(u) { try { return new URL(u).host; } catch { return u; } }

// --- add to panel (share) ---------------------------------------------

const shareDialog = document.getElementById("share-dialog");
let shareTarget = null;

// openShareDialog asks for one confirmation and nothing else: the login goes to
// the panel this machine is joined to, as the person it joined as. A machine
// joined to no panel is told what to do instead of shown a form.
async function openShareDialog(account, login, there) {
  let st = null;
  try { st = await api("/api/panel"); } catch (_) {}
  if (!st || !st.enrolled) {
    alert("This machine isn't connected to a panel yet.\n\nAsk the panel's admin for an invite link and paste it under “Got an invite?” — after that, adding a login is one click.");
    return;
  }
  shareTarget = { account, login };
  const who = login.email || account.name;
  document.getElementById("share-title").textContent = there ? `Refresh ${who} on the panel` : `Add ${who} to the panel`;
  document.getElementById("share-note").textContent = there
    ? `Hands a fresh login for ${there.account} up to ${prettyURL(st.server)}. Everyone sharing it keeps their access; use this after reconnecting a login that stopped working.`
    : `Adds this login to ${prettyURL(st.server)} as ${st.personName || "you"}, so people you give access to can use it through the gateway, and you can take it back any time. Important: from then on run it through the gateway (its command under “Shared with you”), not this local one — using the same login both ways breaks it for everyone.`;
  document.getElementById("share-err").textContent = "";
  document.getElementById("share-go").textContent = there ? "Refresh it" : "Add it";
  shareDialog.showModal();
}

document.getElementById("share-cancel").addEventListener("click", () => shareDialog.close());
document.getElementById("share-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!shareTarget) return;
  const err = document.getElementById("share-err");
  err.textContent = "";
  const go = document.getElementById("share-go");
  const label = go.textContent;
  go.disabled = true; go.textContent = "Adding…";
  try {
    const out = await api("/api/logins/add-to-panel", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        configDir: shareTarget.account.configDir || "",
        name: shareTarget.account.name || "",
        email: shareTarget.login.email || "",
        plan: shareTarget.login.plan || "",
      }),
    });
    shareDialog.close();
    await refreshPanel();
    refreshAccounts();
    if (!out.refreshed) {
      alert(`${out.added} is on the panel. It's under “Shared with you” here — run it from there from now on, not as the local "${shareTarget.account.name}".\n\nOpen the panel to give other people access.`);
    }
  } catch (e2) {
    err.textContent = e2.message;
  } finally {
    go.disabled = false; go.textContent = label;
  }
});

// --- sign in another / rename -----------------------------------------

const nameDialog = document.getElementById("name-dialog");
const renameDialog = document.getElementById("rename-dialog");

function openNameDialog() {
  document.getElementById("name-input").value = "";
  nameDialog.showModal();
}
document.getElementById("signin-btn").addEventListener("click", openNameDialog);
document.getElementById("signin-first").addEventListener("click", openNameDialog);
document.getElementById("name-cancel").addEventListener("click", () => nameDialog.close());
document.getElementById("name-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = document.getElementById("name-input").value.trim();
  if (!name) return;
  let account;
  try {
    account = await api("/api/accounts", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
  } catch (err) {
    refreshAccounts();
    alert("Could not create the account: " + err.message);
    return;
  }
  nameDialog.close();
  refreshAccounts();
  startLogin(account);
});

document.getElementById("rename-cancel").addEventListener("click", () => renameDialog.close());
document.getElementById("rename-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const id = document.getElementById("rename-id").value;
  const name = document.getElementById("rename-name").value.trim();
  if (!name) return;
  try {
    await api(`/api/accounts/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
  } catch (err) {
    refreshAccounts();
    alert("Could not rename: " + err.message);
    return;
  }
  renameDialog.close();
  refreshAccounts();
});

document.getElementById("connect-btn").addEventListener("click", connectWithInvite);
document.getElementById("invite-input").addEventListener("keydown", (e) => {
  if (e.key === "Enter") connectWithInvite();
});

// --- login flow -------------------------------------------------------

const loginDialog = document.getElementById("login-dialog");
const loginTitle = document.getElementById("login-title");
const loginStatus = document.getElementById("login-status");
const loginUrlBox = document.getElementById("login-url-box");
const loginUrlLink = document.getElementById("login-url");
const loginCodeForm = document.getElementById("login-code-form");
const loginCodeInput = document.getElementById("login-code");

let activeEventSource = null;
let activeLoginAccountId = null;
let loginFinished = false;

function startLogin(account) {
  activeLoginAccountId = account.id;
  loginFinished = false;
  loginTitle.textContent = `Sign in ${account.name}`;
  loginStatus.textContent = "Starting…";
  loginUrlBox.hidden = true;
  loginCodeForm.hidden = true;
  loginCodeInput.value = "";
  if (!loginDialog.open) loginDialog.showModal();

  api(`/api/accounts/${account.id}/login`, { method: "POST" })
    .then(() => openLoginStream(account.id))
    .catch((err) => { loginStatus.textContent = "Could not start: " + err.message; });
}

function openLoginStream(accountId) {
  if (accountId !== activeLoginAccountId) return;
  if (activeEventSource) activeEventSource.close();
  const es = new EventSource(`/api/accounts/${accountId}/login/events`);
  activeEventSource = es;
  es.addEventListener("message", (e) => {
    let event;
    try { event = JSON.parse(e.data); } catch (_) { return; }
    handleLoginEvent(event);
  });
  es.addEventListener("error", () => {
    if (es.readyState === EventSource.CLOSED && !loginFinished) {
      loginStatus.textContent = "Lost the connection to clawdh. Close this and try again.";
    }
  });
}

function handleLoginEvent(event) {
  switch (event.type) {
    case "url":
      loginStatus.textContent = "Open this link to finish signing in:";
      loginUrlBox.hidden = false;
      loginCodeForm.hidden = false;
      loginUrlLink.href = event.url;
      loginUrlLink.textContent = event.url;
      break;
    case "linked": finishLogin("Signed in. This account is ready."); break;
    case "timeout": finishLogin("Timed out waiting for the sign-in. You can try again."); break;
    case "failed": finishLogin("Sign-in failed: " + (event.message || "unknown error")); break;
  }
}

function finishLogin(message) {
  loginFinished = true;
  loginStatus.textContent = message;
  loginUrlBox.hidden = true;
  loginCodeForm.hidden = true;
  if (activeEventSource) activeEventSource.close();
  refreshAccounts();
}

loginCodeForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const code = loginCodeInput.value.trim();
  if (!code || !activeLoginAccountId) return;
  try {
    await api(`/api/accounts/${activeLoginAccountId}/login/code`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ code }),
    });
    loginStatus.textContent = "Code submitted, finishing…";
    loginCodeInput.value = "";
  } catch (err) {
    loginStatus.textContent = "Could not submit that code: " + err.message;
  }
});

document.getElementById("login-copy").addEventListener("click", (e) => {
  if (loginUrlLink.href) copyToClipboard(loginUrlLink.href, e.currentTarget);
});
document.getElementById("login-close").addEventListener("click", () => loginDialog.close());

loginDialog.addEventListener("close", () => {
  if (activeEventSource) { activeEventSource.close(); activeEventSource = null; }
  if (activeLoginAccountId && !loginFinished) cancelLogin(activeLoginAccountId);
  activeLoginAccountId = null;
});

function cancelLogin(accountId) {
  const path = `/api/accounts/${accountId}/login/cancel`;
  if (navigator.sendBeacon && navigator.sendBeacon(path, new Blob([], { type: "text/plain" }))) return;
  api(path, { method: "POST", keepalive: true }).catch(() => {});
}
window.addEventListener("pagehide", () => {
  if (activeLoginAccountId && !loginFinished) cancelLogin(activeLoginAccountId);
});

// --- staying current --------------------------------------------------

async function loadServerInfo() {
  try {
    const status = await api("/api/status");
    if (status && status.tag) buildTag.textContent = status.tag;
  } catch (_) {}
}

let polling = false;
async function poll() {
  if (polling) return;
  polling = true;
  try {
    await loadServerInfo();
    await loadLogins();
    await loadAccounts();
    await refreshPanel();
    // Last: it offers the accounts and shares the two reads above just loaded.
    if (typeof refreshSessions === "function") await refreshSessions();
    refreshedLabel.textContent = "updated " + clockTime(new Date());
  } catch (err) {
    refreshedLabel.textContent = "could not refresh: " + err.message;
  } finally {
    polling = false;
  }
}

poll().catch((err) => { accountsList.textContent = "Could not load accounts: " + err.message; });
setInterval(poll, POLL_MS);
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") poll();
});
