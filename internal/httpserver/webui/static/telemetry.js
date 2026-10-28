// telemetry.js — "Your usage": what this person's Claude Code sessions used, at
// a glance. Shown once this machine is connected to a panel, which is where the
// numbers come from: the gateway meters every request, and the panel answers for
// the person this machine is enrolled as, and no one else.
//
// There is one choice to make, and only one: every session, or one of them. The
// rest is laid out so it reads without switching anything — how much, over the
// spans people think in; what it went on, in plain words; and when. Nothing is
// estimated: every figure is the gateway's own count, in the one unit plans
// count in ("usage"), with the raw token counts beside it in the chart's tooltip.
//
// It stays cheap. Every session: the minutes are read once and then only the
// newest is asked for every 10 seconds; the hours and days, a few dozen points
// each, are read again once a minute. One session is a few dozen points at most,
// read again every 15 seconds. A hidden tab asks for nothing.
(function () {
  "use strict";

  var block = document.getElementById("telemetry-block");
  if (!block) return;
  var focusEl = document.getElementById("tel-focus");
  var errEl = document.getElementById("tel-err");
  var tilesEl = document.getElementById("tel-tiles");
  var mixEl = document.getElementById("tel-mix");
  var mixSpanEl = document.getElementById("tel-mix-span");
  var mixRowsEl = document.getElementById("tel-mix-rows");
  var mixSayEl = document.getElementById("tel-mix-say");
  var chartTitleEl = document.getElementById("tel-chart-title");
  var plotEl = document.getElementById("tel-plot");
  var sessionsEl = document.getElementById("tel-sessions");

  // The kinds of token, bottom of a stack first. Colour means the kind and
  // nothing else; every colour is named beside it wherever it appears.
  var CLASSES = [
    { key: "cacheRead", label: "Re-reading the conversation", color: "#9085e9",
      say: "Claude re-reading the conversation on every request. It grows as a conversation gets longer." },
    { key: "cacheWrite", label: "Adding new context", color: "#199e70",
      say: "adding new context: files, results and messages Claude hasn't seen yet." },
    { key: "input", label: "New input", color: "#3987e5",
      say: "new input that could not be served from the cache." },
    { key: "output", label: "Claude writing", color: "#d95926",
      say: "Claude writing: its answers, code and tool calls." }
  ];
  var MIN = 60e3, HOUR = 3600e3, DAY = 864e5;
  var STEP = { minute: MIN, hour: HOUR, day: DAY };
  // How far back each grain's series reaches; mirrors panel.Resolutions.
  var SPAN = { minute: 2 * HOUR, hour: 72 * HOUR, day: 30 * DAY };

  var enrolled = false;
  var session = ""; // "" is every session; otherwise one session's id
  var sessions = [];
  var showAllSessions = false;
  var minutes = [], hours = [], days = [], sessionPts = [], sessionGrain = "minute";
  var loaded = false;
  var timers = [];

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function svg(name, attrs) {
    var e = document.createElementNS("http://www.w3.org/2000/svg", name);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function fmt(n) {
    n = Math.round(n || 0);
    if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
    return String(n);
  }
  function startOf(t, g) { return Math.floor(t / STEP[g]) * STEP[g]; }
  function part(p, key) { return (p && p[key] && p[key].weighted) || 0; }
  function raw(p, key) { return (p && p[key] && p[key].tokens) || 0; }
  function sum(points, from, f) {
    return points.reduce(function (a, p) { return Date.parse(p.t) >= from ? a + f(p) : a; }, 0);
  }
  function ago(ms) {
    var s = Math.max(0, (Date.now() - ms) / 1000);
    if (s < 90) return "just now";
    if (s < 5400) return Math.round(s / 60) + " min ago";
    if (s < 129600) return Math.round(s / 3600) + " h ago";
    return Math.round(s / 86400) + " days ago";
  }
  function span(ms) {
    var m = Math.round(ms / MIN);
    if (m < 1) return "under a minute";
    if (m < 60) return m + " min";
    var h = Math.floor(m / 60), r = m % 60;
    if (h < 48) return h + " h" + (r ? " " + r + " min" : "");
    return Math.round(h / 24) + " days";
  }
  function sessionName(x) { return x.title || (x.here ? "Untitled session" : "Ran on another machine"); }
  function current() { return sessions.filter(function (s) { return s.id === session; })[0]; }

  function tile(label, value, note) {
    var t = el("div", "tel-tile");
    t.append(el("div", "tel-tile-l", label), el("div", "tel-tile-v", value), el("div", "tel-tile-n", note || ""));
    return t;
  }

  // --- the four tiles --------------------------------------------------------

  function drawTiles() {
    var now = Date.now();
    if (!session) {
      var hourFrom = startOf(now, "minute") - 59 * MIN;
      var w = function (p) { return p.weighted; };
      tilesEl.replaceChildren(
        tile("Last hour", fmt(sum(minutes, hourFrom, w)), fmt(sum(minutes, hourFrom, function (p) { return p.requests || 0; })) + " requests"),
        tile("Last 24 hours", fmt(sum(hours, startOf(now, "hour") - 23 * HOUR, w))),
        tile("Last 7 days", fmt(sum(days, startOf(now, "day") - 6 * DAY, w))),
        tile("Last 30 days", fmt(sum(days, startOf(now, "day") - 29 * DAY, w)))
      );
      return;
    }
    var x = current(), u = (x && x.usage) || {};
    var first = x ? Date.parse(x.first) : 0, last = x ? Date.parse(x.last) : 0;
    tilesEl.replaceChildren(
      tile("Total", fmt(u.weighted), "usage"),
      tile("Requests", fmt(u.requests), "subagents included"),
      tile("Per request", u.requests ? fmt(u.weighted / u.requests) : "—", "on average"),
      tile("Active", x ? span(last - first) : "—", x ? "last request " + ago(last) : "")
    );
  }

  // --- what it's spent on ----------------------------------------------------

  function drawMix() {
    var totals, label;
    if (!session) {
      var from = startOf(Date.now(), "hour") - 23 * HOUR;
      totals = CLASSES.map(function (c) { return sum(hours, from, function (p) { return part(p, c.key); }); });
      label = "· last 24 hours";
    } else {
      var u = (current() || {}).usage;
      totals = CLASSES.map(function (c) { return part(u, c.key); });
      label = "· whole session";
    }
    mixSpanEl.textContent = label;
    var all = totals.reduce(function (a, b) { return a + b; }, 0);
    mixEl.replaceChildren();
    mixRowsEl.replaceChildren();
    mixSayEl.textContent = "";
    mixEl.setAttribute("aria-label", all > 0 ? CLASSES.map(function (c, i) { return c.label + " " + Math.round(totals[i] / all * 100) + "%"; }).join(", ") : "Nothing used yet");
    if (all <= 0) {
      mixRowsEl.appendChild(el("p", "tel-note", loaded ? "Nothing was used in this span." : "Reading…"));
      return;
    }
    CLASSES.forEach(function (c, i) {
      if (totals[i] > 0) {
        var seg = el("span");
        seg.style.flexGrow = String(totals[i]);
        seg.style.background = c.color;
        mixEl.appendChild(seg);
      }
    });
    // The rows read biggest first; the bar keeps the fixed order.
    var order = CLASSES.map(function (c, i) { return i; }).sort(function (a, b) { return totals[b] - totals[a]; });
    order.forEach(function (i) {
      var c = CLASSES[i], row = el("div", "tel-mix-row");
      var sw = el("span", "tel-swatch");
      sw.style.background = c.color;
      row.append(sw, el("span", "tel-mix-name", c.label), el("span", "tel-mix-pct", pct(totals[i], all)), el("span", "tel-mix-v", fmt(totals[i])));
      mixRowsEl.appendChild(row);
    });
    var top = CLASSES[order[0]], share = totals[order[0]] / all;
    mixSayEl.textContent = (share >= 0.5 ? "Most of it" : "The biggest part") + " (" + pct(totals[order[0]], all) + ") is " + top.say;
  }
  function pct(v, all) {
    var p = v / all * 100;
    return (p > 0 && p < 1 ? "<1" : String(Math.round(p))) + "%";
  }

  // --- the chart -------------------------------------------------------------

  function chartView() {
    var now = Date.now();
    if (!session) return { grain: "minute", from: startOf(now - SPAN.minute, "minute"), until: now, points: minutes, title: "Last 2 hours · per minute" };
    var x = current(), first = x ? Date.parse(x.first) : now, last = x ? Math.min(now, Date.parse(x.last)) : now;
    var words = { minute: "per minute", hour: "per hour", day: "per day" };
    return { grain: sessionGrain, from: startOf(first, sessionGrain), until: last, points: sessionPts, title: "This session · " + words[sessionGrain] };
  }
  // 12-hour with am/pm in the viewer's own zone, like every other time here.
  function clock12(d) {
    return d.toLocaleTimeString("en-US", { hour: "numeric", minute: "2-digit", hour12: true }).toLowerCase();
  }
  function label(ms, g) {
    var d = new Date(ms);
    if (g === "minute") return clock12(d);
    if (g === "hour") return d.toLocaleDateString("en-US", { weekday: "short" }) + " " + clock12(d);
    return d.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
  }
  function niceMax(v) {
    if (v <= 0) return 1;
    var p = Math.pow(10, Math.floor(Math.log10(v))), n = v / p;
    var steps = [1, 1.5, 2, 3, 4, 5, 6, 8, 10];
    for (var i = 0; i < steps.length; i++) if (n <= steps[i]) return steps[i] * p;
    return 10 * p;
  }

  function drawChart() {
    var v = chartView(), now = Date.now();
    chartTitleEl.textContent = v.title;
    plotEl.replaceChildren();
    var times = [];
    for (var t = v.from; t <= v.until; t += STEP[v.grain]) times.push(t); // a session's chart ends where it did
    var by = {};
    v.points.forEach(function (p) { by[Date.parse(p.t)] = p; });
    var w = plotEl.clientWidth || 640, H = 200, padL = 48, padR = 6, padT = 8, padB = 24;
    var max = niceMax(Math.max.apply(null, [0].concat(times.map(function (t) { return by[t] ? by[t].weighted : 0; }))));
    var slot = (w - padL - padR) / Math.max(times.length, 1);
    var bw = Math.max(1, slot - (slot > 4 ? 1.5 : 0));
    var y = function (val) { return padT + (H - padT - padB) * (1 - val / max); };
    var every = Math.max(1, Math.ceil(times.length / Math.max(2, Math.floor((w - padL) / 80))));

    var s = svg("svg", { width: w, height: H, viewBox: "0 0 " + w + " " + H, role: "img", "aria-label": v.title + ", usage per bucket, by kind of token" });
    [0, 0.5, 1].forEach(function (f, i) {
      var val = f * max;
      s.appendChild(svg("line", { x1: padL, x2: w - padR, y1: y(val), y2: y(val), "class": i === 0 ? "tel-axis" : "tel-grid" }));
      var tx = svg("text", { x: padL - 8, y: y(val) + 4, "text-anchor": "end", "class": "tel-tick" });
      tx.textContent = val === 0 ? "0" : fmt(val).replace(".0", "");
      s.appendChild(tx);
    });
    times.forEach(function (t, i) {
      var p = by[t], x = padL + i * slot + (slot - bw) / 2, acc = 0;
      if (p) {
        var segs = CLASSES.map(function (c) { return { v: part(p, c.key), color: c.color }; }).filter(function (sg) { return sg.v > 0; });
        // A panel older than the split sends totals only: draw those, uncoloured.
        if (!segs.length && p.weighted > 0) segs = [{ v: p.weighted, color: "#7C8797" }];
        segs.forEach(function (sg, j) {
          var top = y(acc + sg.v), bottom = y(acc);
          acc += sg.v;
          var gap = j > 0 && bottom - top > 3 ? 1 : 0; // a 1px surface gap between parts, where there is room
          s.appendChild(svg("rect", { x: x, y: top, width: bw, height: Math.max(j === 0 ? 1 : 0, bottom - top - gap), rx: bw > 5 && j === segs.length - 1 ? 2 : 0, fill: sg.color }));
        });
      }
      if (i % every === 0) {
        var lt = svg("text", { x: x + bw / 2, y: H - 7, "text-anchor": "middle", "class": "tel-tick" });
        lt.textContent = label(t, v.grain);
        s.appendChild(lt);
      }
    });

    var tip = el("div", "tel-tip");
    tip.hidden = true;
    s.addEventListener("mousemove", function (e) {
      var r = s.getBoundingClientRect(), i = Math.floor((e.clientX - r.left - padL) / slot);
      if (i < 0 || i >= times.length) { tip.hidden = true; return; }
      var p = by[times[i]];
      tip.replaceChildren(el("b", null, label(times[i], v.grain)));
      CLASSES.slice().reverse().forEach(function (c) {
        var row = el("div", "tel-tip-row"), sw = el("span", "tel-swatch");
        sw.style.background = c.color;
        row.append(sw, el("span", "tel-tip-m", c.label), el("span", "tel-tip-v", fmt(part(p, c.key))), el("span", "tel-tip-raw", fmt(raw(p, c.key)) + " tok"));
        tip.appendChild(row);
      });
      var all = el("div", "tel-tip-row tel-tip-all");
      all.append(el("span", "tel-tip-m", "Total"), el("span", "tel-tip-v", fmt(p ? p.weighted : 0)), el("span", "tel-tip-raw", fmt(p ? p.tokens : 0) + " tok"));
      tip.appendChild(all);
      if (p && p.requests) tip.appendChild(el("div", "tel-tip-m", p.requests + (p.requests === 1 ? " request" : " requests")));
      tip.style.left = Math.min(Math.max(padL + i * slot - 115, 0), Math.max(0, w - 240)) + "px";
      tip.hidden = false;
    });
    s.addEventListener("mouseleave", function () { tip.hidden = true; });
    plotEl.append(s, tip);
    if (loaded && !v.points.length) plotEl.appendChild(el("div", "tel-empty", "Nothing was used in this span."));
  }

  // --- sessions (the one choice) ---------------------------------------------

  function drawSessions() {
    sessionsEl.replaceChildren();
    if (!sessions.length) {
      sessionsEl.appendChild(el("p", "tel-note", "No session has been recorded yet. Every request still counts above."));
      return;
    }
    var shown = showAllSessions ? sessions : sessions.slice(0, 6);
    shown.forEach(function (x) {
      var u = x.usage || {}, on = session === x.id;
      var b = el("button", "tel-session" + (on ? " is-on" : ""));
      b.type = "button";
      b.setAttribute("role", "listitem");
      b.setAttribute("aria-pressed", String(on));
      b.title = x.id;
      var name = el("div", "tel-session-name");
      name.append(el("span", "tel-session-title", sessionName(x)), el("span", "tel-session-proj", [x.project, ago(Date.parse(x.last))].filter(Boolean).join(" · ")));
      var bar = el("div", "tel-session-mix");
      if (!(u.weighted > 0)) bar.classList.add("is-empty");
      CLASSES.forEach(function (c) {
        var v = part(u, c.key);
        if (v <= 0 || !(u.weighted > 0)) return;
        var seg = el("span");
        seg.style.width = (v / u.weighted * 100) + "%";
        seg.style.background = c.color;
        bar.appendChild(seg);
      });
      var nums = el("div", "tel-session-nums");
      nums.append(el("span", "tel-session-v", fmt(u.weighted)), el("span", "tel-session-m", u.requests && u.weighted > 0 ? "≈" + fmt(u.weighted / u.requests) + " per request" : ""));
      b.append(name, bar, nums);
      b.addEventListener("click", function () { pickSession(on ? "" : x.id); });
      sessionsEl.appendChild(b);
    });
    if (sessions.length > 6) {
      var more = el("button", "subtle-btn tel-more", showAllSessions ? "Show fewer" : "Show all " + sessions.length);
      more.type = "button";
      more.addEventListener("click", function () { showAllSessions = !showAllSessions; drawSessions(); });
      sessionsEl.appendChild(more);
    }
  }

  function drawFocus() {
    focusEl.hidden = !session;
    if (!session) return;
    var x = current(), t = el("span", "tel-focus-t");
    t.append("Showing ", el("b", null, x ? sessionName(x) : session.slice(0, 8)));
    var all = el("button", "subtle-btn", "Show all sessions");
    all.type = "button";
    all.addEventListener("click", function () { pickSession(""); });
    focusEl.replaceChildren(t, all);
  }

  function render() {
    drawFocus();
    drawTiles();
    drawMix();
    drawChart();
  }

  // --- reading ---------------------------------------------------------------

  async function get(path) {
    var res = await fetch(path);
    var body = await res.json();
    if (!res.ok) throw new Error(body.error || "That did not work (" + res.status + ").");
    return body;
  }
  function failed(e) {
    errEl.textContent = e.message; // keep what is on screen through a hiccup
    errEl.hidden = false;
  }
  function ok() { errEl.hidden = true; loaded = true; render(); }

  // merge folds an incremental minute read into what is held, dropping what slid out.
  function mergeMinutes(incoming) {
    var by = {};
    minutes.concat(incoming).forEach(function (p) { by[Date.parse(p.t)] = p; });
    var floor = startOf(Date.now() - SPAN.minute, "minute");
    minutes = Object.keys(by).map(Number).filter(function (t) { return t >= floor; }).sort(function (a, b) { return a - b; }).map(function (t) { return by[t]; });
  }

  async function syncMinutes(whole) {
    if (document.hidden || !enrolled || session) return;
    var since = !whole && minutes.length ? minutes[minutes.length - 1].t : "";
    try {
      var body = await get("/api/telemetry?res=minute" + (since ? "&since=" + encodeURIComponent(since) : ""));
      if (session) return;
      mergeMinutes(body.points || []);
      ok();
    } catch (e) { failed(e); }
  }
  async function syncLong() {
    if (document.hidden || !enrolled || session) return;
    try {
      var both = await Promise.all([get("/api/telemetry?res=hour"), get("/api/telemetry?res=day")]);
      if (session) return;
      hours = both[0].points || [];
      days = both[1].points || [];
      ok();
    } catch (e) { failed(e); }
  }
  // The session's grain follows its length, so its whole life is one readable
  // chart; the panel reads a session over its own span, however long ago.
  function grainFor(x) {
    var length = Date.parse(x.last) - Date.parse(x.first);
    return length <= SPAN.minute ? "minute" : length <= SPAN.hour ? "hour" : "day";
  }
  async function syncSession() {
    if (document.hidden || !enrolled || !session) return;
    var asked = session, x = current();
    if (!x) return;
    var g = grainFor(x);
    try {
      var body = await get("/api/telemetry?res=" + g + "&session=" + encodeURIComponent(asked));
      if (asked !== session) return; // an answer for a session since left
      sessionGrain = g;
      sessionPts = body.points || [];
      ok();
    } catch (e) { failed(e); }
  }
  async function loadSessions() {
    if (document.hidden || !enrolled) return;
    try {
      var body = await get("/api/telemetry/sessions");
      sessions = body.sessions || [];
      if (session && !current()) session = ""; // it slid out of the last 30 days
      drawSessions();
      if (loaded) render();
    } catch (e) { sessionsEl.replaceChildren(el("p", "row-err", e.message)); }
  }

  function every(fn, ms) { timers.push(setInterval(fn, ms)); }
  function stopAll() { timers.forEach(clearInterval); timers = []; }

  function start() {
    stopAll();
    loaded = false;
    render();
    if (!session) {
      syncMinutes(true);
      syncLong();
      every(function () { syncMinutes(false); }, 10000);
      every(syncLong, 60000);
    } else {
      sessionPts = [];
      syncSession();
      every(syncSession, 15000);
    }
    every(loadSessions, 30000);
  }

  function pickSession(id) {
    session = id;
    drawSessions();
    start();
    if (id) block.scrollIntoView({ behavior: "smooth", block: "start" });
  }

  // Connected or not is the panel's say; ask now and then, and start the first
  // time the answer is yes.
  async function checkEnrolled() {
    try {
      var st = await get("/api/panel");
      var was = enrolled;
      enrolled = !!st.enrolled;
      block.hidden = !enrolled;
      if (enrolled && !was) { start(); loadSessions(); }
      if (!enrolled && was) { stopAll(); minutes = hours = days = sessionPts = []; }
    } catch (e) { /* the rest of the page reports connection trouble */ }
  }
  checkEnrolled();
  setInterval(checkEnrolled, 15000);
  document.addEventListener("visibilitychange", function () {
    if (document.hidden || !enrolled) return;
    loadSessions();
    if (session) syncSession(); else { syncMinutes(false); syncLong(); }
  });
  window.addEventListener("resize", function () { if (enrolled) drawChart(); });
})();
