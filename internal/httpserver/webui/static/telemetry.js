// telemetry.js — "Your usage": this person's own usage over time, as finely as a
// second or as coarsely as a month. Shown once this machine is connected to a
// panel, which is where the numbers come from: the gateway meters every request,
// and the panel answers for the person this machine is enrolled as, and no one
// else.
//
// Nothing is estimated. A bar is what was metered in its bucket — weighted tokens
// or raw tokens, every class stacked or one class alone; a bucket with nothing in
// it is an empty slot. The fine grains are cut from
// individual requests (so they carry a request count); the longer ones come from
// the hourly totals the gateway keeps with them and agree with them to the token.
//
// It stays cheap by asking for less each time. The whole view is read once; after
// that a sync sends the end of what is already held and gets back the newest
// bucket, anything that has slid out of the view is dropped, and a hidden tab
// asks for nothing — so a page left open all day holds the same few hundred
// points it started with.
(function () {
  "use strict";

  var block = document.getElementById("telemetry-block");
  if (!block) return;
  var grainsEl = document.getElementById("tel-grains");
  var showEl = document.getElementById("tel-show");
  var measureEl = document.getElementById("tel-measure");
  var legendEl = document.getElementById("tel-legend");
  var sessionsEl = document.getElementById("tel-sessions");
  var focusEl = document.getElementById("tel-focus");
  var statsEl = document.getElementById("tel-stats");
  var plotEl = document.getElementById("tel-plot");
  var noteEl = document.getElementById("tel-note");
  var errEl = document.getElementById("tel-err");

  var GRAINS = [["second", "Seconds"], ["minute", "Minutes"], ["hour", "Hours"], ["day", "Days"], ["week", "Weeks"], ["month", "Months"], ["all", "All time"]];
  // How far back each grain reaches, how long a bucket is, how often it syncs.
  // The reach and length mirror panel.Resolutions; months have no fixed length.
  var SPAN = { second: 5 * 60e3, minute: 2 * 3600e3, hour: 72 * 3600e3, day: 30 * 864e5, week: 26 * 7 * 864e5, month: 730 * 864e5, all: 0 };
  var STEP = { second: 1e3, minute: 60e3, hour: 3600e3, day: 864e5, week: 7 * 864e5, month: 0, all: 0 };
  var SYNC = { second: 3000, minute: 10000, hour: 30000, day: 60000, week: 60000, month: 60000, all: 60000 };

  // The token classes, bottom of a stack first. Colour means the class and
  // nothing else, and every class is named beside its swatch (the same four hues
  // the panel uses, checked for colour vision against this surface).
  var CLASSES = [
    { key: "cacheRead", label: "Cache read", color: "#9085e9", note: "the conversation so far, re-read from cache on every request" },
    { key: "cacheWrite", label: "Cache write", color: "#199e70", note: "new context written to the cache" },
    { key: "input", label: "Input", color: "#3987e5", note: "uncached input" },
    { key: "output", label: "Output", color: "#d95926", note: "what the model wrote" }
  ];
  var SHOW = [["all", "All"], ["input", "Input"], ["output", "Output"], ["cacheWrite", "Cache write"], ["cacheRead", "Cache read"]];
  var MEASURES = [["weighted", "Weighted"], ["tokens", "Tokens"]];
  var REST = "#7C8797"; // what a panel older than the split could not put in a class

  var grain = "minute";
  var session = ""; // "" is every session; otherwise one session's id
  var sessions = [];
  var showAllSessions = false;
  var show = remembered("tel-show", "all", SHOW);
  var measure = remembered("tel-measure", "weighted", MEASURES);
  var held = [];
  var now = Date.now();
  var loaded = false;
  var enrolled = false;
  var timer = 0;
  var hover = null;

  // remembered keeps a viewer's choice across visits; a browser that keeps
  // nothing just starts from the default.
  function remembered(key, fallback, allowed) {
    try {
      var v = localStorage.getItem(key);
      return allowed.some(function (a) { return a[0] === v; }) ? v : fallback;
    } catch (e) { return fallback; }
  }
  function remember(key, v) { try { localStorage.setItem(key, v); } catch (e) { /* nothing to keep it in */ } }

  // value is a bucket's figure for a class (or all of them) in a measure.
  function value(p, cls, m) {
    if (!p) return 0;
    if (cls === "all") return m === "weighted" ? p.weighted : p.tokens;
    return (p[cls] && p[cls][m]) || 0;
  }
  function showLabel(k) { for (var i = 0; i < SHOW.length; i++) if (SHOW[i][0] === k) return SHOW[i][1]; return k; }
  function classOf(k) { for (var i = 0; i < CLASSES.length; i++) if (CLASSES[i].key === k) return CLASSES[i]; return null; }

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

  // startOf cuts an instant to its bucket in UTC, the way the panel does: weeks
  // begin on Monday and a month on the 1st.
  function startOf(t, g) {
    if (g === "week") {
      var day = Math.floor(t / 864e5) * 864e5;
      return day - ((new Date(day).getUTCDay() + 6) % 7) * 864e5;
    }
    if (g === "month" || g === "all") { var d = new Date(t); return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1); }
    return Math.floor(t / STEP[g]) * STEP[g];
  }
  function nextBucket(t, g) {
    if (g === "month" || g === "all") { var d = new Date(t); return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1); }
    return t + STEP[g];
  }
  // slots is every bucket the view covers, so a quiet stretch shows as the gap it is.
  function slots() {
    var first = grain === "all" ? startOf(held.length ? Date.parse(held[0].t) : now, grain) : startOf(now - SPAN[grain], grain);
    var out = [];
    for (var t = first; t <= now; t = nextBucket(t, grain)) out.push(t);
    return out;
  }
  // merge folds a sync into what is held: the newest bucket replaces its older
  // self, anything new is added, anything that slid out of the view goes.
  function merge(incoming) {
    var by = {};
    held.forEach(function (p) { by[Date.parse(p.t)] = p; });
    incoming.forEach(function (p) { by[Date.parse(p.t)] = p; });
    var floor = grain === "all" ? -Infinity : startOf(now - SPAN[grain], grain);
    held = Object.keys(by).map(Number).filter(function (t) { return t >= floor; }).sort(function (a, b) { return a - b; }).map(function (t) { return by[t]; });
  }

  function fmt(n) {
    n = Math.round(n || 0);
    if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1) + "K";
    return String(n);
  }
  function label(ms) {
    var d = new Date(ms);
    if (grain === "second") return d.toLocaleTimeString("en-GB");
    if (grain === "minute") return d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
    if (grain === "hour") return d.toLocaleDateString("en-US", { weekday: "short" }) + " " + d.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
    if (grain === "day" || grain === "week") return d.toLocaleDateString("en-US", { month: "short", day: "numeric", timeZone: "UTC" });
    return d.toLocaleDateString("en-US", { month: "short", year: "numeric", timeZone: "UTC" });
  }
  function niceMax(v) {
    if (v <= 0) return 1;
    var p = Math.pow(10, Math.floor(Math.log10(v))), n = v / p;
    return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p;
  }

  function stat(l, value, note) {
    var s = el("div", "tel-stat");
    s.append(el("div", "tel-stat-l", l), el("div", "tel-stat-v", value), el("div", "tel-stat-n", note || ""));
    return s;
  }

  function render() {
    var total = 0, requests = 0, peak = null, peakV = -1;
    held.forEach(function (p) {
      var v = value(p, show, measure);
      total += v; requests += p.requests || 0;
      if (v > peakV) { peak = p; peakV = v; }
    });
    var raw = grain === "second" || grain === "minute";
    var unit = measure === "weighted" ? "weighted tokens" : "tokens";
    statsEl.replaceChildren(
      stat("In view", fmt(total), show === "all" ? unit + ", every class" : showLabel(show).toLowerCase() + ", " + unit),
      stat("Busiest", peak ? fmt(peakV) : "—", peak ? label(Date.parse(peak.t)) : ""),
      stat("Requests", raw ? fmt(requests) : "—", raw ? "counted per request" : "not counted at this grain"),
      stat("Per request", raw && requests ? fmt(total / requests) : "—", raw ? "on average, in view" : "not counted at this grain")
    );
    drawLegend();
    noteEl.textContent = "Every request re-sends the conversation so far, so cache reads grow with the context and usually lead. " +
      "Weighted puts every class on one scale: output counts 5×, cache writes 1.25× and cache reads 0.1× an input token, times the model's weight. " +
      "Seconds and minutes are cut from individual requests. Hours and longer come from the hourly totals the gateway keeps with them." +
      (raw || grain === "hour" ? "" : " Days, weeks and months are UTC; weeks begin on Monday.");
    drawChart();
  }

  // drawLegend names every class with its total and share in view; picking one
  // shows it alone, picking it again goes back to all of them.
  function drawLegend() {
    var totals = CLASSES.map(function (c) { return held.reduce(function (a, p) { return a + ((p[c.key] && p[c.key][measure]) || 0); }, 0); });
    var sum = totals.reduce(function (a, b) { return a + b; }, 0);
    legendEl.replaceChildren();
    CLASSES.slice().reverse().forEach(function (c) {
      var i = CLASSES.indexOf(c), on = show === c.key;
      var b = el("button", "tel-class" + (show !== "all" && !on ? " is-dim" : ""));
      b.type = "button";
      b.title = c.note;
      b.setAttribute("aria-pressed", String(on));
      var sw = el("span", "tel-swatch");
      sw.style.background = c.color;
      b.append(sw, el("span", "tel-class-l", c.label), el("span", "tel-class-v", fmt(totals[i])), el("span", "tel-class-p", sum > 0 ? Math.round(totals[i] / sum * 100) + "%" : ""));
      b.addEventListener("click", function () { pickShow(on ? "all" : c.key); });
      legendEl.appendChild(b);
    });
  }

  // segments is a bucket's bar, bottom up: one class, or every class stacked with
  // whatever no class accounts for on top in grey.
  function segments(p) {
    if (!p) return [];
    if (show !== "all") return [{ v: value(p, show, measure), color: classOf(show).color }];
    var out = CLASSES.map(function (c) { return { v: (p[c.key] && p[c.key][measure]) || 0, color: c.color }; });
    var all = value(p, "all", measure), rest = all - out.reduce(function (a, s) { return a + s.v; }, 0);
    if (rest > Math.max(1, all * 1e-6)) out.push({ v: rest, color: REST });
    return out.filter(function (s) { return s.v > 0; });
  }

  function drawChart() {
    plotEl.replaceChildren();
    var w = plotEl.clientWidth || 640, H = 220, padL = 46, padR = 6, padT = 10, padB = 24;
    var times = slots();
    var by = {};
    held.forEach(function (p) { by[Date.parse(p.t)] = p; });
    var vals = times.map(function (t) { return value(by[t], show, measure); });
    var max = niceMax(Math.max.apply(null, [0].concat(vals)));
    var slot = (w - padL - padR) / Math.max(times.length, 1);
    var bw = Math.max(1, slot - (slot > 4 ? 1.5 : 0));
    var y = function (v) { return padT + (H - padT - padB) * (1 - v / max); };
    var every = Math.max(1, Math.ceil(times.length / Math.max(2, Math.floor((w - padL) / 92))));

    var s = svg("svg", { width: w, height: H, viewBox: "0 0 " + w + " " + H, role: "img", "aria-label": (show === "all" ? "Every token class" : showLabel(show)) + ", " + (measure === "weighted" ? "weighted" : "tokens") + ", per bucket" });
    [0, 0.25, 0.5, 0.75, 1].forEach(function (f, i) {
      var v = f * max;
      s.appendChild(svg("line", { x1: padL, x2: w - padR, y1: y(v), y2: y(v), "class": i === 0 ? "tel-axis" : "tel-grid" }));
      var t = svg("text", { x: padL - 8, y: y(v) + 4, "text-anchor": "end", "class": "tel-tick" });
      t.textContent = v === 0 ? "0" : fmt(v);
      s.appendChild(t);
    });
    times.forEach(function (t, i) {
      var x = padL + i * slot + (slot - bw) / 2, acc = 0, segs = segments(by[t]);
      segs.forEach(function (sg, j) {
        var top = y(acc + sg.v), bottom = y(acc);
        acc += sg.v;
        var gap = j > 0 && bottom - top > 3 ? 1 : 0; // a 1px surface gap between stacked parts, where there is room
        s.appendChild(svg("rect", { x: x, y: top, width: bw, height: Math.max(j === 0 ? 1 : 0, bottom - top - gap), rx: bw > 5 && j === segs.length - 1 ? 2 : 0, fill: sg.color, "class": "tel-bar" }));
      });
      if (i % every === 0) {
        var tx = svg("text", { x: x + bw / 2, y: H - 7, "text-anchor": "middle", "class": "tel-tick" });
        tx.textContent = label(t);
        s.appendChild(tx);
      }
    });
    var tip = el("div", "tel-tip");
    tip.hidden = true;
    s.addEventListener("mousemove", function (e) {
      var r = s.getBoundingClientRect(), i = Math.floor((e.clientX - r.left - padL) / slot);
      if (i < 0 || i >= times.length) { tip.hidden = true; return; }
      var p = by[times[i]];
      var unit = measure === "weighted" ? "weighted" : "tokens";
      tip.replaceChildren(el("b", null, label(times[i])));
      CLASSES.slice().reverse().forEach(function (c) {
        var row = el("div", "tel-tip-row" + (show === "all" || show === c.key ? "" : " is-dim") + (show === c.key ? " is-on" : ""));
        var sw = el("span", "tel-swatch");
        sw.style.background = c.color;
        row.append(sw, el("span", "tel-tip-m", c.label), el("span", "tel-tip-v", fmt(p && p[c.key] ? p[c.key][measure] : 0)));
        tip.appendChild(row);
      });
      var all = el("div", "tel-tip-row tel-tip-all" + (show === "all" ? " is-on" : ""));
      all.append(el("span", "tel-tip-m", "All, " + unit), el("span", "tel-tip-v", fmt(value(p, "all", measure))));
      tip.appendChild(all);
      if (p && p.requests) tip.appendChild(el("div", "tel-tip-m", p.requests + (p.requests === 1 ? " request" : " requests")));
      tip.style.left = Math.min(Math.max(padL + i * slot - 95, 0), Math.max(0, w - 210)) + "px";
      tip.hidden = false;
    });
    s.addEventListener("mouseleave", function () { tip.hidden = true; });
    plotEl.append(s, tip);
    if (loaded && !held.length) plotEl.appendChild(el("div", "tel-empty", "Nothing was metered in this view."));
  }

  async function sync(whole) {
    if (document.hidden || !enrolled) return;
    var since = !whole && held.length ? held[held.length - 1].t : "";
    var asked = grain;
    try {
      var askedSession = session;
      var res = await fetch("/api/telemetry?res=" + asked + (since ? "&since=" + encodeURIComponent(since) : "") + (session ? "&session=" + encodeURIComponent(session) : ""));
      var body = await res.json();
      if (!res.ok) throw new Error(body.error || "That did not work (" + res.status + ").");
      if (asked !== grain || askedSession !== session) return; // a slower answer for a view since left
      now = Date.now();
      merge(body.points || []);
      loaded = true;
      errEl.hidden = true;
      render();
    } catch (e) {
      errEl.textContent = e.message; // keep what is on screen through a hiccup
      errEl.hidden = false;
    }
  }

  function ago(ms) {
    var s = Math.max(0, (Date.now() - ms) / 1000);
    if (s < 90) return "just now";
    if (s < 5400) return Math.round(s / 60) + " min ago";
    if (s < 129600) return Math.round(s / 3600) + " h ago";
    return Math.round(s / 86400) + " days ago";
  }
  function sessionName(x) { return x.title || (x.here ? "Untitled session" : "Ran on another machine"); }

  // drawSessions lists this person's sessions, newest first, each with what it
  // came to in the chosen measure and how much of that was cache reads.
  function drawSessions() {
    sessionsEl.replaceChildren();
    if (!sessions.length) {
      sessionsEl.appendChild(el("p", "tel-note", "No session has been metered yet. The gateway records which session a request came from once it runs a build that does; until then every request still counts above, just under no session."));
      return;
    }
    var shown = showAllSessions ? sessions : sessions.slice(0, 8);
    shown.forEach(function (x) {
      var u = x.usage || {}, on = session === x.id;
      var all = value(u, "all", measure), read = (u.cacheRead && u.cacheRead[measure]) || 0;
      var b = el("button", "tel-session" + (on ? " is-on" : ""));
      b.type = "button";
      b.setAttribute("role", "listitem");
      b.setAttribute("aria-pressed", String(on));
      b.title = x.id;
      var name = el("div", "tel-session-name");
      name.append(el("span", "tel-session-title", sessionName(x)), el("span", "tel-session-proj", [x.project, ago(Date.parse(x.last))].filter(Boolean).join(" · ")));
      var bar = el("div", "tel-session-mix");
      CLASSES.forEach(function (c) {
        var v = (u[c.key] && u[c.key][measure]) || 0;
        if (v <= 0 || all <= 0) return;
        var seg = el("span");
        seg.style.width = (v / all * 100) + "%";
        seg.style.background = c.color;
        bar.appendChild(seg);
      });
      var nums = el("div", "tel-session-nums");
      nums.append(el("span", "tel-session-v", fmt(all) + (measure === "weighted" ? " weighted" : " tokens")),
        el("span", "tel-session-m", fmt(u.requests || 0) + " requests · " + (all > 0 ? Math.round(read / all * 100) : 0) + "% cache reads"));
      b.append(name, bar, nums);
      b.addEventListener("click", function () { pickSession(on ? "" : x.id); });
      sessionsEl.appendChild(b);
    });
    if (sessions.length > 8) {
      var more = el("button", "subtle-btn tel-more", showAllSessions ? "Show fewer" : "Show all " + sessions.length);
      more.type = "button";
      more.addEventListener("click", function () { showAllSessions = !showAllSessions; drawSessions(); });
      sessionsEl.appendChild(more);
    }
  }

  function drawFocus() {
    var x = sessions.filter(function (s) { return s.id === session; })[0];
    focusEl.hidden = !session;
    if (!session) return;
    focusEl.replaceChildren(el("span", "tel-focus-l", "Session"), el("b", null, x ? sessionName(x) : session.slice(0, 8)));
    var clear = el("button", "subtle-btn", "Show every session");
    clear.type = "button";
    clear.addEventListener("click", function () { pickSession(""); });
    focusEl.appendChild(clear);
  }

  function pickSession(id) {
    session = id;
    held = [];
    loaded = false;
    drawFocus();
    drawSessions();
    render();
    sync(true);
  }

  async function loadSessions() {
    if (document.hidden || !enrolled) return;
    try {
      var res = await fetch("/api/telemetry/sessions");
      var body = await res.json();
      if (!res.ok) throw new Error(body.error || "The sessions could not be read (" + res.status + ").");
      sessions = body.sessions || [];
      drawSessions();
      drawFocus();
    } catch (e) {
      sessionsEl.replaceChildren(el("p", "row-err", e.message));
    }
  }

  function schedule() {
    clearInterval(timer);
    timer = setInterval(function () { sync(false); }, SYNC[grain]);
  }

  function pick(g) {
    grain = g;
    held = [];
    loaded = false;
    Array.prototype.forEach.call(grainsEl.children, function (b) { b.setAttribute("aria-pressed", String(b.dataset.grain === g)); });
    render();
    sync(true);
    schedule();
  }

  function pressed(group, v) {
    Array.prototype.forEach.call(group.children, function (b) { b.setAttribute("aria-pressed", String(b.dataset.v === v)); });
  }
  function pickShow(v) { show = v; remember("tel-show", v); pressed(showEl, v); render(); }
  function pickMeasure(v) { measure = v; remember("tel-measure", v); pressed(measureEl, v); render(); drawSessions(); }
  [[showEl, SHOW, pickShow, show], [measureEl, MEASURES, pickMeasure, measure]].forEach(function (g) {
    g[1].forEach(function (o) {
      var b = el("button", "tel-grain", o[1]);
      b.type = "button";
      b.dataset.v = o[0];
      b.setAttribute("aria-pressed", String(o[0] === g[3]));
      b.addEventListener("click", function () { g[2](o[0]); });
      g[0].appendChild(b);
    });
  });

  GRAINS.forEach(function (g) {
    var b = el("button", "tel-grain", g[1]);
    b.type = "button";
    b.dataset.grain = g[0];
    b.addEventListener("click", function () { pick(g[0]); });
    grainsEl.appendChild(b);
  });

  // Connected or not is the panel's say; ask now and then, and start syncing the
  // first time the answer is yes.
  async function checkEnrolled() {
    try {
      var res = await fetch("/api/panel");
      var st = await res.json();
      var was = enrolled;
      enrolled = !!st.enrolled;
      block.hidden = !enrolled;
      if (enrolled && !was) { pick(grain); loadSessions(); }
      if (!enrolled && was) { clearInterval(timer); held = []; }
    } catch (e) { /* the rest of the page reports connection trouble */ }
  }
  checkEnrolled();
  setInterval(checkEnrolled, 15000);
  setInterval(loadSessions, 30000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) { checkEnrolled(); if (enrolled) { sync(false); loadSessions(); } } });
  window.addEventListener("resize", function () { if (enrolled) drawChart(); });
})();
