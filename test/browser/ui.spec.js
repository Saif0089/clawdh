// End-to-end tests that drive clawdh's actual web UI in a real browser
// against a real clawdh server, with testdata/fakeclaude standing in for
// the `claude` CLI.
const { test, expect } = require("@playwright/test");
const { spawn, execFileSync } = require("child_process");
const fs = require("fs");
const http = require("http");
const net = require("net");
const os = require("os");
const path = require("path");

const repoRoot = path.resolve(__dirname, "..", "..");
const isWindows = process.platform === "win32";
const exe = (name) => (isWindows ? `${name}.exe` : name);

let server;
let baseURL;
// pageURL is baseURL with the first-run intro turned off (see beforeEach).
let pageURL;
let workDir;
let usageServer;

// A stand-in for Anthropic's usage endpoint. Reset times are relative so
// the countdowns in the page are always in the future, whenever the
// suite happens to run.
function usagePayload() {
  const inHours = (h) => new Date(Date.now() + h * 3600_000).toISOString();
  return JSON.stringify({
    limits: [
      { kind: "session", group: "session", percent: 12, severity: "normal", resets_at: inHours(3), is_active: true },
      { kind: "weekly_all", group: "weekly", percent: 94, severity: "normal", resets_at: inHours(50), is_active: true },
      { kind: "weekly_scoped", group: "weekly", percent: 71, severity: "normal", resets_at: inHours(50), is_active: true, scope: { model: { display_name: "Fable" } } },
    ],
    extra_usage: { is_enabled: false },
  });
}

// The one token this stub refuses, so a test can put an account in the
// "the API rejected this login" state without expiring anything.
const REJECTED_TOKEN = "revoked-access-token";

// The one token this stub is slow to answer, standing in for a call to
// Anthropic that takes its time: longer than two poll intervals, and
// shorter than the 15 seconds clawdh allows the call.
const SLOW_TOKEN = "slow-access-token";
const SLOW_ANSWER_MS = 12_000;

function startUsageStub() {
  return new Promise((resolve) => {
    const srv = http.createServer((req, res) => {
      const auth = req.headers.authorization || "";
      if (auth.includes(REJECTED_TOKEN)) {
        res.writeHead(401, { "Content-Type": "application/json" });
        res.end("{}");
        return;
      }
      if (auth.includes(SLOW_TOKEN)) {
        setTimeout(() => {
          res.writeHead(200, { "Content-Type": "application/json" });
          res.end(usagePayload());
        }, SLOW_ANSWER_MS);
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(usagePayload());
    });
    srv.listen(0, "127.0.0.1", () => resolve(srv));
  });
}

// writeCredentials plants a login for an account directly, so a test can set
// the short access token and the long refresh window independently — enough to
// drive an account into "Ready" or "Sign-in expired".
function writeCredentials(configDir, { accessToken, accessInHours, refreshInDays }) {
  fs.writeFileSync(
    path.join(configDir, ".credentials.json"),
    JSON.stringify({
      claudeAiOauth: {
        accessToken,
        refreshToken: "fake-refresh-token",
        expiresAt: Date.now() + accessInHours * 3600_000,
        refreshTokenExpiresAt: Date.now() + refreshInDays * 24 * 3600_000,
        subscriptionType: "max",
        rateLimitTier: "default_claude_max_20x",
      },
    })
  );
}

async function createAccount(name) {
  const res = await fetch(`${baseURL}/api/accounts`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: baseURL },
    body: JSON.stringify({ name }),
  });
  expect(res.ok).toBeTruthy();
  return res.json();
}

async function deleteAccount(id) {
  const res = await fetch(`${baseURL}/api/accounts/${id}`, {
    method: "DELETE",
    headers: { Origin: baseURL },
  });
  expect(res.ok).toBeTruthy();
}

// setVisibility tells the page its tab was hidden or shown. A headless
// browser never hides a page by itself, so the state is overridden and
// the event dispatched the way the browser would dispatch it.
async function setVisibility(page, state) {
  await page.evaluate((s) => {
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => s });
    Object.defineProperty(document, "hidden", { configurable: true, get: () => s === "hidden" });
    document.dispatchEvent(new Event("visibilitychange"));
  }, state);
}

function build(pkg, outPath) {
  execFileSync("go", ["build", "-o", outPath, pkg], { cwd: repoRoot, stdio: "inherit" });
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.unref();
    srv.on("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

async function waitForServer(url, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${url}/api/status`);
      if (res.ok) return;
    } catch (_) {
      // not up yet
    }
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`clawdh did not start within ${timeoutMs}ms`);
}

test.beforeAll(async () => {
  workDir = fs.mkdtempSync(path.join(os.tmpdir(), "clawdh-browser-"));
  const home = path.join(workDir, "home");
  fs.mkdirSync(home);

  const clawdhBin = path.join(workDir, exe("clawdh"));
  const fakeClaude = path.join(workDir, exe("claude"));
  build("./cmd/clawdh", clawdhBin);
  build("./testdata/fakeclaude", fakeClaude);

  const port = await freePort();
  baseURL = `http://127.0.0.1:${port}`;

  usageServer = await startUsageStub();
  const usageURL = `http://127.0.0.1:${usageServer.address().port}/usage`;

  server = spawn(clawdhBin, ["serve", "--port", String(port)], {
    env: {
      ...process.env,
      HOME: home,
      USERPROFILE: home,
      APPDATA: path.join(home, "AppData", "Roaming"),
      LOCALAPPDATA: path.join(home, "AppData", "Local"),
      CLAWDH_CLAUDE_BIN: fakeClaude,
      // Never let the suite call Anthropic for real.
      CLAWDH_USAGE_ENDPOINT: usageURL,
      // Keep the "here is your URL" state on screen long enough to be
      // asserted on; the fake otherwise finishes in ~300ms and the UI
      // races straight past it to "Connected".
      FAKECLAUDE_LOGIN_DELAY_MS: "2500",
    },
    stdio: "inherit",
  });

  await waitForServer(baseURL, 30_000);
  pageURL = `${baseURL}/?notour=1`;
});

test.afterAll(async () => {
  if (server) server.kill();
  if (usageServer) usageServer.close();
});

// Fail any test that logs a page error or a console error: the bug that
// prompted these tests surfaced first as a thrown SyntaxError.
test.beforeEach(async ({ page }) => {
  // The first-run intro auto-opens over the page and blocks clicks (a real user
  // dismisses it). These tests drive the page directly, so every goto below
  // carries ?notour=1 — see pageURL. It used to be suppressed by writing the
  // "seen" key into localStorage, but that key carries a version, and when the
  // version was bumped the tests went on writing the old one: the overlay came
  // back and every click in this file landed on it instead of the page.
  page.on("pageerror", (err) => {
    throw new Error(`uncaught page error: ${err.message}`);
  });
  page.on("console", (msg) => {
    if (msg.type() === "error") {
      throw new Error(`console error: ${msg.text()}`);
    }
  });
});

test("adds an account, shows the full OAuth URL, and links it", async ({ page }) => {
  await page.goto(pageURL);

  await page.click("#signin-btn");
  await page.fill("#name-input", "Work");
  await page.click("#name-form button[type=submit]");

  // The regression this whole file exists for: the login dialog used to
  // show "Could not start login: The string did not match the expected
  // pattern." here, and later never rendered the URL at all because the
  // SSE payload's field names didn't match what app.js reads.
  await expect(page.locator("#login-status")).toContainText("Open this link");

  const url = await page.locator("#login-url").textContent();
  expect(url).toMatch(/^https:\/\//);
  // A URL truncated at the pseudo-terminal's width is a link that
  // simply fails; the real one is ~600 characters.
  expect(url.length).toBeGreaterThan(400);
  expect(url).not.toContain(" ");

  await expect(page.locator("#login-status")).toContainText("Signed in");

  await page.click("#login-close");
  await expect(page.locator(".status-pill")).toHaveText("Ready");
  await expect(page.locator(".run-cmd")).toHaveText("clawdh work");
});

test("shows plan usage and reset countdowns", async ({ page }) => {
  await page.goto(pageURL);

  const meters = page.locator(".meter");
  await expect(meters).toHaveCount(3);

  await expect(meters.nth(0).locator(".meter-label")).toHaveText("Current session");
  await expect(meters.nth(1).locator(".meter-label")).toHaveText("This week, all models");
  await expect(meters.nth(2).locator(".meter-label")).toHaveText("This week, Fable");

  await expect(meters.nth(0).locator(".meter-pct")).toHaveText("12%");
  await expect(meters.nth(1).locator(".meter-pct")).toHaveText("94%");
  await expect(meters.nth(2).locator(".meter-pct")).toHaveText("71%");

  // Colour carries one meaning on this page: how much headroom is left.
  // If these classes stop tracking the numbers, a nearly-exhausted week
  // renders in the same calm green as an untouched one.
  await expect(meters.nth(0)).toHaveClass(/level-ok/);
  await expect(meters.nth(1)).toHaveClass(/level-crit/);
  await expect(meters.nth(2)).toHaveClass(/level-warn/);

  // The bar has to actually move; a fill left at zero width would look
  // identical for every account. It grows over a few frames, and a renderer
  // with no GPU (CI) can take a moment to produce the first one — so poll.
  await expect
    .poll(() => meters.nth(1).locator(".bar-fill").evaluate((el) => el.getBoundingClientRect().width), { timeout: 5000 })
    .toBeGreaterThan(0);

  // One line per window now: how long is left sits beside the percent, and
  // the exact moment is the title.
  await expect(meters.nth(0).locator(".meter-reset")).toHaveText(/\d+h \d+m left/);
  await expect(meters.nth(1).locator(".meter-reset")).toHaveText(/2d \d+h left/);

  // A working login says so with the Ready pill and nothing more: the old
  // pair of ticking clocks (a long "login session" and a short "access
  // token") only ever got confused for each other, so a healthy account
  // shows no countdown line at all.
  await expect(page.locator(".session-line")).toBeHidden();

  await expect(page.locator(".plan-chip")).toHaveText("Max plan");
  await expect(page.locator(".status-pill")).toHaveText("Ready");
});

// The page has no refresh button any more, so the only thing that keeps
// it honest is that it re-reads by itself. If this stops, nothing on
// screen changes and nothing looks broken — it just quietly shows
// yesterday's numbers.
test("re-reads usage on its own, with no button to press", async ({ page }) => {
  const account = await createAccount("Polling");
  writeCredentials(account.configDir, { accessToken: "fake-access-token", accessInHours: 8, refreshInDays: 27 });

  const usageRequests = [];
  page.on("request", (req) => {
    if (req.url().includes(`/accounts/${account.id}/usage`)) usageRequests.push(req.url());
  });

  await page.goto(pageURL);
  await expect(page.locator("#refresh-btn")).toHaveCount(0);

  const card = page.locator(".card", { hasText: "Polling" });
  await expect(card.locator(".meter").first()).toBeVisible();
  const afterLoad = usageRequests.length;
  expect(afterLoad).toBeGreaterThan(0);

  // Two poll intervals plus room for a slow machine.
  await page.waitForTimeout(11_000);
  expect(usageRequests.length).toBeGreaterThan(afterLoad);

  // And the numbers must not flicker back to zero on every poll: the
  // bar is still its full width after several refreshes.
  const width = await card
    .locator(".meter")
    .nth(1)
    .locator(".bar-fill")
    .evaluate((el) => el.getBoundingClientRect().width);
  expect(width).toBeGreaterThan(0);

  await deleteAccount(account.id);
});

// Which build is answering, in the corner of the page. It is the only
// way to tell a fix that shipped from a fix that is actually running —
// the question that comes up every time an update lands silently.
test("names the running build in the header", async ({ page }) => {
  await page.goto(pageURL);
  const tag = page.locator("#build-tag");
  await expect(tag).toBeVisible();
  // A version, a commit, or both — never empty.
  await expect(tag).toHaveText(/\S/);

  const status = await (await fetch(`${baseURL}/api/status`)).json();
  await expect(tag).toHaveText(status.tag);
});

// An account clawdh knows about but has no login for must say so, rather
// than keep showing the last status it saw.
test("reports a signed-out account instead of claiming it is linked", async ({ page }) => {
  const created = await fetch(`${baseURL}/api/accounts`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: baseURL },
    body: JSON.stringify({ name: "Never Connected" }),
  });
  expect(created.ok).toBeTruthy();

  await page.goto(pageURL);
  const card = page.locator(".card", { hasText: "Never Connected" });
  await expect(card.locator(".status-pill")).toHaveText("Signed out");
  await expect(card.locator(".meter")).toHaveCount(0);
  await expect(card.locator(".usage-note")).toBeVisible();

  const removed = await fetch(`${baseURL}/api/accounts/${(await created.json()).id}`, {
    method: "DELETE",
    headers: { Origin: baseURL },
  });
  expect(removed.ok).toBeTruthy();
});

// The bug: the account being used at that very moment showed the red
// "login expired" badge. Claude Code only refreshes the short access
// token when it runs, so between runs clawdh held a stale one, the API
// answered 401, and a working login was reported as rejected.
test("calls an account with a stale access token linked, not expired", async ({ page }) => {
  const account = await createAccount("Stale Token");
  // The stale token is one the API refuses, exactly as a real expired
  // one is: the fix is that clawdh never sends it in the first place.
  writeCredentials(account.configDir, { accessToken: REJECTED_TOKEN, accessInHours: -1, refreshInDays: 27 });

  await page.goto(pageURL);
  const card = page.locator(".card", { hasText: "Stale Token" });
  await expect(card.locator(".status-pill")).toHaveText("Ready");
  await expect(card.locator(".meter")).toHaveCount(0);
  await expect(card.locator(".usage-note")).toContainText("updates next time you run");
  // Ready, so nothing tells the person to reconnect — the login is fine, it
  // is only the short-lived access token that lapsed, and that renews itself.
  await expect(card.locator(".session-line")).toBeHidden();

  await deleteAccount(account.id);
});

// A login the API really does reject is expired, and the card says so in one
// actionable line rather than a confusing countdown to a future date.
test("tells you to reconnect a login the API has rejected", async ({ page }) => {
  const account = await createAccount("Revoked Login");
  writeCredentials(account.configDir, { accessToken: REJECTED_TOKEN, accessInHours: 8, refreshInDays: 27 });

  await page.goto(pageURL);
  const card = page.locator(".card", { hasText: "Revoked Login" });
  await expect(card.locator(".status-pill")).toHaveText("Sign-in expired");
  await expect(card.locator(".usage-note")).toContainText("rejected");
  await expect(card.locator(".session-line")).toBeVisible();
  await expect(card.locator(".session-line")).toContainText("Reconnect");

  await deleteAccount(account.id);
});

// One slow account used to hold up the whole page: the poll waited for
// every card's numbers, so the other cards stopped updating and the poll
// skipped its ticks until the slow answer came back.
test("a slow account does not hold up the other cards", async ({ page }) => {
  const slow = await createAccount("Slow Answer");
  writeCredentials(slow.configDir, { accessToken: SLOW_TOKEN, accessInHours: 8, refreshInDays: 27 });
  const quick = await createAccount("Quick Answer");
  writeCredentials(quick.configDir, { accessToken: "fake-access-token", accessInHours: 8, refreshInDays: 27 });

  let quickRequests = 0;
  let slowRequests = 0;
  page.on("request", (req) => {
    if (req.url().includes(`/accounts/${quick.id}/usage`)) quickRequests++;
    if (req.url().includes(`/accounts/${slow.id}/usage`)) slowRequests++;
  });

  const loaded = Date.now();
  await page.goto(pageURL);
  const quickCard = page.locator(".card", { hasText: "Quick Answer" });
  await expect(quickCard.locator(".meter").first()).toBeVisible();

  // The next tick asks the quick account again while the slow answer is
  // still out. Waiting on it would push that request past SLOW_ANSWER_MS.
  await expect.poll(() => quickRequests, { timeout: 9_000 }).toBeGreaterThan(1);
  // That tick leaves the slow card alone: it is still waiting on its first
  // answer, and asking again on top would pile requests up behind it. The
  // tick sends every card's request together, so a moment is enough to
  // see a second one if it went.
  await page.waitForTimeout(500);
  expect(slowRequests).toBe(1);
  expect(Date.now() - loaded).toBeLessThan(SLOW_ANSWER_MS);
  await expect(page.locator(".card", { hasText: "Slow Answer" }).locator(".meter")).toHaveCount(0);
  // And the poll itself finished without it.
  await expect(page.locator("#refreshed")).toContainText("updated", { timeout: 1000 });

  await deleteAccount(slow.id);
  await deleteAccount(quick.id);
});

// Numbers that stopped updating look exactly like numbers that did not
// change, unless the card says how old they are. The browser's clock is
// moved on rather than the server's numbers aged: to the page, a read
// minutes behind its own clock is the same thing.
test("says how old a card's numbers are once they fall behind", async ({ page }) => {
  const account = await createAccount("Aging Numbers");
  writeCredentials(account.configDir, { accessToken: "fake-access-token", accessInHours: 8, refreshInDays: 27 });

  // Every usage request for this card passes through here, so the test
  // can tell when none is out, and hold one when it needs to.
  const usagePath = `/api/accounts/${account.id}/usage`;
  let outstanding = 0;
  let holding = false;
  let hold;
  const held = new Promise((resolve) => (hold = resolve));
  const isUsage = (req) => new URL(req.url()).pathname === usagePath;
  page.on("request", (req) => isUsage(req) && outstanding++);
  page.on("requestfinished", (req) => isUsage(req) && outstanding--);
  page.on("requestfailed", (req) => isUsage(req) && outstanding--);
  await page.route(
    (url) => url.pathname === usagePath,
    (route) => (holding ? hold(route) : route.continue())
  );

  await page.clock.install();
  await page.goto(pageURL);
  const card = page.locator(".card", { hasText: "Aging Numbers" });
  const age = card.locator(".usage-age");
  await expect(card.locator(".meter").first()).toBeVisible();
  // Numbers just read need no caption.
  await expect(age).toBeHidden();

  // Stop the page's clock, so no poll runs but the ones this test moves
  // it through, and let any usage request already out come back.
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000));
  await expect.poll(() => outstanding).toBe(0);
  // Marked, so a meter rebuilt to move the label would be noticed.
  await card.locator(".meter").first().evaluate((el) => (el.dataset.original = "yes"));

  // The next usage request is held, so from here on no answer from the
  // server can be what moves the label: only the page's own clock can.
  holding = true;
  await page.clock.fastForward("03:30");
  const route = await held;
  await expect(age).toBeVisible();
  await expect(age).toHaveText(/^as of .+ · 3 min ago$/);

  // It keeps counting by itself, and leaves the meters alone doing so.
  await page.clock.runFor(60_000);
  await expect(age).toHaveText(/ · 4 min ago$/);
  await expect(card.locator('.meter[data-original="yes"]')).toHaveCount(1);

  // A fresh read takes the caption away again. The same numbers under a
  // different note do not rebuild the meters either: a stale card's note
  // says how old its numbers are, so it changes every minute while they
  // stay put.
  const response = await route.fetch();
  const snapshot = await response.json();
  snapshot.usage.fetchedAt = await page.evaluate(() => new Date().toISOString());
  snapshot.error = "A note that changed while the numbers did not.";
  await route.fulfill({ response, json: snapshot });
  await expect(age).toBeHidden();
  await expect(card.locator(".usage-note")).toHaveText(snapshot.error);
  await expect(card.locator('.meter[data-original="yes"]')).toHaveCount(1);

  // The clock is still stopped, so no poll can ask for this account's
  // usage once it is gone: that would be a 404, which the console
  // listener above fails the test on.
  await deleteAccount(account.id);
});

// A hidden tab's timers are throttled, so a page left behind another
// window came back out of date and stayed that way until its next tick.
test("reads everything again as soon as a hidden tab is shown", async ({ page }) => {
  await page.clock.install();
  await page.goto(pageURL);
  // The first poll has come back; no account is needed for that.
  await expect(page.locator("#refreshed")).toContainText("updated");

  // Stop the page's clock so no scheduled poll can be what answers
  // below, and let any poll already out come back first.
  await page.clock.pauseAt(await page.evaluate(() => Date.now() + 1000));
  await page.waitForTimeout(1000);

  const isAccountList = (req) => new URL(req.url()).pathname === "/api/accounts";
  let listRequests = 0;
  page.on("request", (req) => {
    if (isAccountList(req)) listRequests++;
  });

  // Going away is no reason to read anything.
  await setVisibility(page, "hidden");
  await page.waitForTimeout(500);
  expect(listRequests).toBe(0);

  const reread = page.waitForRequest(isAccountList, { timeout: 2000 });
  await setVisibility(page, "visible");
  await reread;
});

// Closing the dialog mid-login must leave the UI able to start another
// one. The state machine behind that (activeLoginAccountId /
// loginFinished / the EventSource) is easy to leave out of sync, and the
// symptom is a dialog that opens showing a stale error and never renders
// the new URL.
test("can start another login after closing one mid-flight", async ({ page }) => {
  await page.goto(pageURL);

  await page.click(".kebab-btn");
  await page.click(".connect-btn");
  await expect(page.locator("#login-status")).toContainText("Open this link");
  await page.click("#login-close");
  // Give the cancel time to actually leave the browser before teardown.
  await page.waitForTimeout(1000);
  await expect(page.locator("#login-dialog")).not.toBeVisible();

  await page.click(".kebab-btn");
  await page.click(".connect-btn");
  await expect(page.locator("#login-status")).toContainText("Open this link");
  const url = await page.locator("#login-url").textContent();
  expect(url.length).toBeGreaterThan(400);
  await page.click("#login-close");
  await page.waitForTimeout(1000);
});

test("renaming an account changes its name but keeps its run command (the slug is stable)", async ({ page }) => {
  await page.goto(pageURL);

  await page.click(".kebab-btn");
  await page.click(".rename-btn");
  await page.fill("#rename-name", "Side Project");
  await page.click("#rename-form button[type=submit]");

  await expect(page.locator(".account-name")).toHaveText("Side Project");
  // The run command is the account's slug, which never changes on rename: its
  // directory (and, on macOS, its Keychain login) are keyed by it, so it can't
  // move. The account is still run as `clawdh work`.
  await expect(page.locator(".run-cmd")).toHaveText("clawdh work");
});

test("rejects a whitespace-only name instead of silently doing nothing", async ({ page }) => {
  await page.goto(pageURL);

  let alerted = "";
  page.on("dialog", async (dialog) => {
    alerted = dialog.message();
    await dialog.dismiss();
  });

  await page.click("#signin-btn");
  await page.fill("#name-input", "   ");
  await page.click("#name-form button[type=submit]");

  // Either the browser blocks it via the pattern, or our own check
  // alerts — what must NOT happen is the dialog closing with nothing
  // created and no explanation.
  await expect(page.locator("#name-dialog")).toBeVisible();
  if (alerted) expect(alerted).toContain("empty");
});

test("removes an account and returns to the empty state", async ({ page }) => {
  await page.goto(pageURL);

  page.on("dialog", (dialog) => dialog.accept());
  await page.click(".kebab-btn");
  await page.click(".remove-btn");

  await expect(page.locator("#accounts-empty")).toBeVisible();
  await expect(page.locator(".card")).toHaveCount(0);
});
