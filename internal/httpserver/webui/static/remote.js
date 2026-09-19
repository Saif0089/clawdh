// remote.js — the "Remote help" section: the owner's side of the consent
// channel. Shows whether remote help is on, the requests the panel has made
// that are waiting for a decision, an "allow for a while" window so a debugging
// session doesn't need a click per request, and what was decided recently.
// Polled alongside the rest of the page.
(function () {
  "use strict";

  var block = document.getElementById("remote-block");
  var toggle = document.getElementById("remote-toggle");
  var toggleText = document.getElementById("remote-toggle-text");
  var body = document.getElementById("remote-body");
  var modeEl = document.getElementById("remote-mode");
  var pendingEl = document.getElementById("remote-pending");
  var historyWrap = document.getElementById("remote-history-wrap");
  var historyEl = document.getElementById("remote-history");
  if (!block) return;

  var options = [];
  var busy = false;
  var lastJSON = ""; // re-render only when the state actually changed, so an open menu isn't yanked away

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  async function call(path, bodyObj) {
    var res = await fetch(path, {
      method: bodyObj === undefined ? "GET" : "POST",
      headers: bodyObj === undefined ? {} : { "Content-Type": "application/json" },
      body: bodyObj === undefined ? undefined : JSON.stringify(bodyObj),
    });
    var data = {};
    try { data = await res.json(); } catch (e) { /* empty */ }
    if (!res.ok) throw new Error(data.error || "That did not work (" + res.status + ").");
    return data;
  }

  function ago(iso) {
    var d = (Date.now() - new Date(iso).getTime()) / 1000;
    if (d < 45) return "just now";
    if (d < 3600) return Math.round(d / 60) + " min ago";
    if (d < 86400) return Math.round(d / 3600) + " h ago";
    return new Date(iso).toLocaleDateString();
  }
  function clock(iso) {
    return new Date(iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
  }
  function leftUntil(iso) {
    var m = Math.max(0, Math.round((new Date(iso).getTime() - Date.now()) / 60000));
    return m >= 60 ? Math.floor(m / 60) + " h " + (m % 60) + " min" : m + " min";
  }

  // trustMenu is the "Allow for…" control: a small popover of the offered windows.
  function trustMenu(label) {
    var wrap = el("div", "menu");
    var btn = el("button", "menu-btn", label);
    btn.type = "button";
    btn.setAttribute("aria-haspopup", "true");
    var items = el("div", "menu-items");
    items.hidden = true;
    options.forEach(function (o) {
      var b = el("button", "menu-item", o.label);
      b.type = "button";
      b.addEventListener("click", function (e) {
        e.stopPropagation();
        items.hidden = true;
        act("/api/remote/trust", { minutes: o.minutes });
      });
      items.appendChild(b);
    });
    btn.addEventListener("click", function (e) {
      e.stopPropagation();
      items.hidden = !items.hidden;
    });
    document.addEventListener("click", function () { items.hidden = true; });
    wrap.appendChild(btn);
    wrap.appendChild(items);
    return wrap;
  }

  async function act(path, bodyObj) {
    if (busy) return;
    busy = true;
    try {
      var v = await call(path, bodyObj || {});
      lastJSON = JSON.stringify(v);
      render(v);
    } catch (e) {
      modeEl.textContent = e.message;
      modeEl.className = "remote-mode err";
    } finally {
      busy = false;
    }
  }

  function render(v) {
    block.hidden = !v.enrolled;
    if (!v.enrolled) return;
    options = v.options || [];
    toggle.checked = !!v.enabled;
    toggleText.textContent = v.enabled ? "On" : "Off";
    body.hidden = !v.enabled;
    if (!v.enabled) return;

    var st = v.state || {};
    var pending = st.pending || [];
    var history = st.history || [];

    // Mode line: asking, or an allow window with an "end now".
    modeEl.replaceChildren();
    modeEl.className = "remote-mode" + (st.trustUntil ? " trusted" : "");
    if (st.trustUntil) {
      var dot = el("span", "mode-dot");
      var t = el("span", null, "Allowing requests without asking until " + clock(st.trustUntil) + " (" + leftUntil(st.trustUntil) + " left).");
      var end = el("button", "link-btn", "End now");
      end.type = "button";
      end.addEventListener("click", function () { act("/api/remote/trust", { minutes: 0 }); });
      modeEl.append(dot, t, end);
    } else {
      var dot2 = el("span", "mode-dot");
      var t2 = el("span", null, "Each request waits for your OK.");
      modeEl.append(dot2, t2, trustMenu("Allow for…"));
    }

    // Held requests.
    pendingEl.replaceChildren();
    if (pending.length === 0) {
      pendingEl.appendChild(el("div", "req-empty", "Nothing is waiting for you."));
    }
    pending.forEach(function (r) {
      var card = el("article", "req-card");
      var main = el("div", "req-main");
      var who = el("div", "req-who");
      var b = el("b", null, r.requestedBy || "The panel");
      who.append(b, document.createTextNode(" asked to "), el("span", "req-what", r.describe || r.kind));
      main.append(who, el("div", "req-meta", ago(r.receivedAt)));
      var actions = el("div", "req-actions");
      var allow = el("button", "primary", "Allow");
      allow.type = "button";
      allow.addEventListener("click", function () { act("/api/remote/requests/" + encodeURIComponent(r.id) + "/allow"); });
      var deny = el("button", "deny", "Deny");
      deny.type = "button";
      deny.addEventListener("click", function () { act("/api/remote/requests/" + encodeURIComponent(r.id) + "/deny"); });
      actions.append(allow, deny, trustMenu("Allow for…"));
      card.append(main, actions);
      pendingEl.appendChild(card);
    });

    // Recent decisions.
    historyWrap.hidden = history.length === 0;
    historyEl.replaceChildren();
    history.slice(0, 8).forEach(function (h) {
      var row = el("div", "hist-row " + h.outcome);
      var mark = el("span", "hist-mark", { allowed: "✓", auto: "✓", denied: "✕", expired: "–" }[h.outcome] || "·");
      var word = { allowed: "Allowed", auto: "Ran (allow window)", denied: "Denied", expired: "No answer — declined" }[h.outcome] || h.outcome;
      var text = el("span", "hist-text");
      text.append(el("b", null, word), document.createTextNode(" · " + (h.requestedBy || "the panel") + " · " + (h.describe || h.kind)));
      row.append(mark, text, el("span", "hist-when", clock(h.decidedAt)));
      historyEl.appendChild(row);
    });
  }

  toggle.addEventListener("change", function () {
    act("/api/remote/enable", { on: toggle.checked });
  });

  async function refresh() {
    try {
      var v = await call("/api/remote");
      var j = JSON.stringify(v);
      if (j === lastJSON) return;
      lastJSON = j;
      render(v);
    } catch (e) {
      /* the rest of the page reports refresh trouble; stay quiet here */
    }
  }
  refresh();
  setInterval(refresh, 5000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) refresh(); });
})();
