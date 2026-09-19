// clawdh web UI — plain JS, no build step. Small enough that a framework
// would cost more than it saves.
"use strict";

const accountsList = document.getElementById("accounts-list");
const accountsEmpty = document.getElementById("accounts-empty");
const rowTemplate = document.getElementById("account-row-template");
const sharedRowTemplate = document.getElementById("shared-row-template");
const meterTemplate = document.getElementById("meter-template");
const refreshedLabel = document.getElementById("refreshed");
const buildTag = document.getElementById("build-tag");

const sharedBlock = document.getElementById("shared-block");
const sharedList = document.getElementById("shared-list");
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
function formatWhen(date) {
  const time = date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const midnight = new Date();
  midnight.setHours(0, 0, 0, 0);
  const dayIndex = Math.floor((date - midnight) / 86400000);
  if (dayIndex === 0) return `today ${time}`;
  if (dayIndex === 1) return `tomorrow ${time}`;
  if (dayIndex > 1 && dayIndex < 7) return `${date.toLocaleDateString([], { weekday: "long" })} ${time}`;
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}
function formatAgo(ms) {
  const minutes = Math.floor(ms / 60000);
  if (minutes < 1) return "less than a minute ago";
  const hours = Math.floor(minutes / 60);
  const mins = minutes % 60;
  if (hours === 0) return `${mins} min ago`;
  return mins > 0 ? `${hours} h ${mins} min ago` : `${hours} h ago`;
}
function fullDate(date) { return date.toLocaleDateString([], { month: "short", day: "numeric" }); }

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
  return JSON.stringify(accounts.map((a) => {
    const l = loginsByDir[a.configDir || ""] || {};
    return [a.id, a.name, a.alias, a.slug, a.status, a.kind, l.email || ""];
  }));
}
let renderedSignature = null;

function renderAccounts(accounts) {
  const signature = listSignature(accounts);
  if (signature === renderedSignature && accountsList.children.length === accounts.length) {
    refreshVisibleUsage(accounts);
    return;
  }
  renderedSignature = signature;
  accountsList.innerHTML = "";
  accountsEmpty.hidden = accounts.length > 0;

  for (const account of accounts) {
    const node = rowTemplate.content.cloneNode(true);
    const card = node.querySelector(".card");
    card.dataset.id = account.id;
    const isDefault = account.kind === "default";
    const login = loginsByDir[account.configDir || ""];

    node.querySelector(".account-name").textContent = account.name;
    node.querySelector(".account-email").textContent = login && login.email ? login.email : "";

    setStatus(node.querySelector(".status-pill"), account.status);

    const cmd = runCommand(account);
    node.querySelector(".run-cmd").textContent = cmd;
    const copyBtn = node.querySelector(".copy-cmd");
    copyBtn.addEventListener("click", () => copyToClipboard(cmd, copyBtn));

    const connectBtn = node.querySelector(".connect-btn");
    connectBtn.textContent = account.status === "linked" ? "Reconnect" : "Connect";
    connectBtn.addEventListener("click", () => startLogin(account));

    // Add to panel: only meaningful once there is a login on this machine to
    // hand over. Kept visible but disabled otherwise, so the path is
    // discoverable without pretending an empty account can be shared.
    const shareBtn = node.querySelector(".share-btn");
    if (login) {
      shareBtn.addEventListener("click", () => openShareDialog(account, login));
    } else {
      shareBtn.disabled = true;
      shareBtn.title = "Sign this account in first, then it can be shared.";
    }

    node.querySelector(".rename-btn").addEventListener("click", () => {
      document.getElementById("rename-id").value = account.id;
      document.getElementById("rename-name").value = account.name;
      renameDialog.showModal();
    });

    const removeBtn = node.querySelector(".remove-btn");
    removeBtn.textContent = isDefault ? "Forget" : "Remove";
    removeBtn.addEventListener("click", () => removeAccount(account, isDefault));

    accountsList.appendChild(node);
    refreshUsage(account, card);
  }
}

function refreshVisibleUsage(accounts) {
  for (const account of accounts) {
    const card = accountsList.querySelector(`.card[data-id="${CSS.escape(account.id)}"]`);
    if (card) refreshUsage(account, card);
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
  const time = at.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  const when = at >= midnight ? time : `${fullDate(at)} ${time}`;
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
  node.querySelector(".meter-pct").textContent = Math.round(percent) + "% used";
  const bar = node.querySelector(".bar");
  bar.setAttribute("aria-valuenow", Math.round(percent));
  bar.setAttribute("aria-label", limit.label);
  const fill = node.querySelector(".bar-fill");
  if (animate) requestAnimationFrame(() => { fill.style.width = percent + "%"; });
  else fill.style.width = percent + "%";
  const reset = node.querySelector(".meter-reset");
  if (limit.resetsAt) {
    countdown(reset, limit.resetsAt, (ms, at) => (ms > 0 ? `resets in ${formatLeft(ms)} (${formatWhen(at)})` : "resetting now"));
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

function renderShared(shares) {
  sharedBlock.hidden = !(shares && shares.length);
  sharedList.innerHTML = "";
  for (const sh of shares || []) {
    const node = sharedRowTemplate.content.cloneNode(true);
    node.querySelector(".account-name").textContent = sh.account;
    const cmd = sharedRunCommand(sh.slug);
    node.querySelector(".run-cmd").textContent = cmd;
    const copyBtn = node.querySelector(".copy-cmd");
    copyBtn.addEventListener("click", () => copyToClipboard(cmd, copyBtn));
    // The gateway's own reading of this account's windows — the same bars a
    // local card draws, so every account shows usage the same way.
    const meters = node.querySelector(".meters");
    if (meters && sh.window) {
      const w = sh.window;
      const limits = [
        { label: "Current session", percent: (w.fiveH || 0) * 100, resetsAt: w.fiveHReset },
        { label: "This week, all models", percent: (w.sevenD || 0) * 100, resetsAt: w.sevenDReset },
      ];
      for (const limit of limits) meters.appendChild(buildMeter(limit, false));
      const note = node.querySelector(".usage-note");
      if (note) { note.hidden = false; note.textContent = "Read through the gateway."; }
    }
    sharedList.appendChild(node);
  }
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
  const raw = document.getElementById("invite-input").value.trim();
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

async function disconnectFromPanel() {
  if (!confirm("Disconnect this machine? Shared accounts stop appearing here.")) return;
  try { await api("/api/panel/disconnect", { method: "POST" }); await refreshPanel(); }
  catch (e) { alert("Could not disconnect: " + e.message); }
}

function prettyURL(u) { try { return new URL(u).host; } catch { return u; } }

// --- add to panel (share) ---------------------------------------------

const shareDialog = document.getElementById("share-dialog");
let shareTarget = null;

function openShareDialog(account, login) {
  shareTarget = { account, login };
  document.getElementById("share-title").textContent = `Share ${account.name}`;
  document.getElementById("share-note").textContent =
    "Adds this login to a panel so many people can use it at once through the gateway. Important: after this, run it only through the gateway (claude-… under “Shared with you”), not this local one — using the same login both ways breaks it for everyone.";
  document.getElementById("share-err").textContent = "";
  document.getElementById("share-password").value = "";
  // Prefill the panel address from wherever this machine is already connected.
  api("/api/panel").then((st) => {
    if (st && st.server) document.getElementById("share-panel").value = st.server;
  }).catch(() => {});
  shareDialog.showModal();
}

document.getElementById("share-cancel").addEventListener("click", () => shareDialog.close());
document.getElementById("share-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  if (!shareTarget) return;
  const panelURL = document.getElementById("share-panel").value.trim();
  const password = document.getElementById("share-password").value;
  const err = document.getElementById("share-err");
  err.textContent = "";
  if (!panelURL || !password) { err.textContent = "Enter the panel address and password."; return; }
  const go = document.getElementById("share-go");
  go.disabled = true; go.textContent = "Adding…";
  try {
    await api("/api/logins/add-to-panel", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        configDir: shareTarget.account.configDir || "",
        email: shareTarget.login.email || "",
        plan: shareTarget.login.plan || "",
        panel: panelURL,
        password,
      }),
    });
    shareDialog.close();
    alert(`${shareTarget.account.name} is on the panel — open it to give people access.\n\nFrom now on, use this account through the gateway (its claude-… command under "Shared with you"), not the local "${shareTarget.account.name}". Running the same login both ways rotates its token and breaks sharing.`);
  } catch (e2) {
    err.textContent = e2.message;
  } finally {
    go.disabled = false; go.textContent = "Add it";
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
    refreshedLabel.textContent = "updated " + new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
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
