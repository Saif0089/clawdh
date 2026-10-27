// telemetry.js — "Your usage": this person's own usage over time, as finely as a
// second or as coarsely as a month. Shown once this machine is connected to a
// panel, which is where the numbers come from: the gateway meters every request,
// and the panel answers for the person this machine is enrolled as, and no one
// else.
//
// Nothing is estimated. A bar is the weighted tokens metered in its bucket; a
// bucket with nothing in it is an empty slot. The fine grains are cut from
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

  var grain = "minute";
  var held = [];
  var now = Date.now();
  var loaded = false;
  var enrolled = false;
  var timer = 0;
  var hover = null;

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
    var total = 0, tokens = 0, requests = 0, peak = null;
    held.forEach(function (p) {
      total += p.weighted; tokens += p.tokens; requests += p.requests || 0;
      if (!peak || p.weighted > peak.weighted) peak = p;
    });
    var raw = grain === "second" || grain === "minute";
    statsEl.replaceChildren(
      stat("In view", fmt(total), "weighted tokens"),
      stat("Raw tokens", fmt(tokens), "every class added up"),
      stat("Busiest", peak ? fmt(peak.weighted) : "—", peak ? label(Date.parse(peak.t)) : ""),
      stat("Requests", raw ? fmt(requests) : "—", raw ? "counted per request" : "not counted at this grain")
    );
    noteEl.textContent = "Seconds and minutes are cut from individual requests. Hours and longer come from the hourly totals the gateway keeps with them." +
      (raw || grain === "hour" ? "" : " Days, weeks and months are UTC; weeks begin on Monday.");
    drawChart();
  }

  function drawChart() {
    plotEl.replaceChildren();
    var w = plotEl.clientWidth || 640, H = 220, padL = 46, padR = 6, padT = 10, padB = 24;
    var times = slots();
    var by = {};
    held.forEach(function (p) { by[Date.parse(p.t)] = p; });
    var vals = times.map(function (t) { return by[t] ? by[t].weighted : 0; });
    var max = niceMax(Math.max.apply(null, [0].concat(vals)));
    var slot = (w - padL - padR) / Math.max(times.length, 1);
    var bw = Math.max(1, slot - (slot > 4 ? 1.5 : 0));
    var y = function (v) { return padT + (H - padT - padB) * (1 - v / max); };
    var every = Math.max(1, Math.ceil(times.length / Math.max(2, Math.floor((w - padL) / 92))));

    var s = svg("svg", { width: w, height: H, viewBox: "0 0 " + w + " " + H, role: "img", "aria-label": "Your weighted tokens metered per bucket" });
    [0, 0.25, 0.5, 0.75, 1].forEach(function (f, i) {
      var v = f * max;
      s.appendChild(svg("line", { x1: padL, x2: w - padR, y1: y(v), y2: y(v), "class": i === 0 ? "tel-axis" : "tel-grid" }));
      var t = svg("text", { x: padL - 8, y: y(v) + 4, "text-anchor": "end", "class": "tel-tick" });
      t.textContent = v === 0 ? "0" : fmt(v);
      s.appendChild(t);
    });
    times.forEach(function (t, i) {
      var v = vals[i], x = padL + i * slot + (slot - bw) / 2;
      if (v > 0) s.appendChild(svg("rect", { x: x, y: y(v), width: bw, height: Math.max(1, H - padB - y(v)), rx: bw > 5 ? 2 : 0, "class": "tel-bar" }));
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
      tip.replaceChildren(el("b", null, label(times[i])), el("div", null, fmt(p ? p.weighted : 0) + " weighted"), el("div", "tel-tip-m", fmt(p ? p.tokens : 0) + " raw tokens"));
      if (p && p.requests) tip.appendChild(el("div", "tel-tip-m", p.requests + (p.requests === 1 ? " request" : " requests")));
      tip.style.left = Math.min(Math.max(padL + i * slot - 60, 0), Math.max(0, w - 160)) + "px";
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
      var res = await fetch("/api/telemetry?res=" + asked + (since ? "&since=" + encodeURIComponent(since) : ""));
      var body = await res.json();
      if (!res.ok) throw new Error(body.error || "That did not work (" + res.status + ").");
      if (asked !== grain) return; // a slower answer for a grain since left
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
      if (enrolled && !was) pick(grain);
      if (!enrolled && was) { clearInterval(timer); held = []; }
    } catch (e) { /* the rest of the page reports connection trouble */ }
  }
  checkEnrolled();
  setInterval(checkEnrolled, 15000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) { checkEnrolled(); if (enrolled) sync(false); } });
  window.addEventListener("resize", function () { if (enrolled) drawChart(); });
})();
