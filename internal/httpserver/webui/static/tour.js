// tour.js — a first-run guided intro for the "This machine" page.
//
// A machine has two kinds of user: someone lending an account (sign a login in,
// add it to a panel) and someone using one (paste an invite, run `clawdh
// shared`). The intro asks which, then walks the few real controls for that path
// — a spotlight on each with a short caption — falling back to a centered card
// when a control isn't on screen yet. The "?" in the corner replays it; first
// run opens it once, remembered with a localStorage flag.
(function () {
  "use strict";

  var SEEN = "clawdh:intro:v2";
  function seen() { try { return localStorage.getItem(SEEN) === "1"; } catch (e) { return false; } }
  function markSeen() { try { localStorage.setItem(SEEN, "1"); } catch (e) { /* private mode */ } }
  var reduce = false;
  try { reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches; } catch (e) { /* old browser */ }

  var TRACKS = {
    // Someone who just wants to use their own accounts here — most often the
    // person who works in VS Code and could not tell whether clawdh applied to
    // them at all, because everything the page said was a terminal command.
    using: [
      { target: "#start-picker", alt: "#run-block", title: "1 · Where new work starts", body: "Pick the account every new session runs as, then press Use for everything. It covers editor chats and terminals alike." },
      { target: "#editor-lane", title: "2 · In an editor", body: "Nothing to install and no command to type. clawdh is already in VS Code and Cursor, so each chat is its own session on the account you chose." },
      { target: "#live-block", alt: "#live-list", title: "3 · Moving what's running", body: "Sessions you have open are listed here. Change the account beside one and that conversation restarts on it, resumed — the others stay put." },
      { title: "That's it", body: "Your accounts and their usage are on the cards below. In a chat you can also type  clawdh <name>  to move just that one." },
    ],
    admin: [
      { target: "#signin-btn", alt: "#signin-first", title: "1 · Sign in the account", body: "Sign in the Claude login you want to lend to your team." },
      { target: ".share-btn", title: "2 · Add it to a panel", body: "Add that login to your team's panel — the gateway hands it out and takes it back, so nobody ever copies your login." },
      { target: "#run-block", title: "3 · Using it yourself", body: "To work on an account here, set it at the top of this page. That covers your editor chats as well as your terminals." },
      { title: "That's it", body: "Then give people access, set quotas and watch usage on the panel." },
    ],
    member: [
      { target: "#invite-input", alt: "#connect-block", title: "1 · Paste your invite", body: "Got a link from someone? Paste it here and hit Connect." },
      { target: "#start-picker", alt: "#run-block", title: "2 · Run it", body: "Pick it at the top of this page and press Use for everything — editor chats and terminals both start on it. In a terminal you can also type  clawdh shared <name>." },
      { target: "#shared-block", title: "Shared with you", body: "Accounts shared with you show up here, each one ready to run." },
    ],
  };
  var ROLE = { title: "Welcome to clawdh", body: "This page runs Claude Code accounts on this machine — in your terminal and in your editor. What brings you here?" };

  var root = null, track = null, idx = 0;

  function elc(tag, cls) { var e = document.createElement(tag); if (cls) e.className = cls; return e; }
  function visible(sel) { if (!sel) return null; var t = document.querySelector(sel); return t && t.offsetParent !== null ? t : null; }
  function targetOf(step) { return visible(step.target) || visible(step.alt); }

  function open() {
    if (root) return;
    track = null; idx = 0;
    root = elc("div", "tour-root" + (reduce ? " tour-reduce" : ""));
    root.setAttribute("role", "dialog");
    root.setAttribute("aria-label", "clawdh intro");
    root.innerHTML = '<div class="tour-spot" aria-hidden="true"></div><div class="tour-dim" aria-hidden="true"></div><div class="tour-card"></div>';
    document.body.appendChild(root);
    render();
    window.addEventListener("resize", onMove);
    window.addEventListener("scroll", onMove, true);
    document.addEventListener("keydown", onKey);
  }
  function close() {
    if (!root) return;
    window.removeEventListener("resize", onMove);
    window.removeEventListener("scroll", onMove, true);
    document.removeEventListener("keydown", onKey);
    root.remove(); root = null; markSeen();
  }
  function onKey(e) {
    if (e.key === "Escape") close();
    else if (track && (e.key === "ArrowRight" || e.key === "Enter")) nextStep();
    else if (track && e.key === "ArrowLeft") prevStep();
  }
  function nextStep() { if (idx >= TRACKS[track].length - 1) close(); else { idx++; render(); } }
  function prevStep() { if (idx > 0) { idx--; render(); } }

  function placeSpot(rect) {
    var spot = root.querySelector(".tour-spot"), dim = root.querySelector(".tour-dim");
    if (rect) {
      var p = 8;
      spot.style.display = "block"; dim.style.display = "none";
      spot.style.top = (rect.top - p) + "px"; spot.style.left = (rect.left - p) + "px";
      spot.style.width = (rect.width + p * 2) + "px"; spot.style.height = (rect.height + p * 2) + "px";
    } else {
      spot.style.display = "none"; dim.style.display = "block";
    }
  }
  function positionCard(rect) {
    var card = root.querySelector(".tour-card");
    var cw = Math.min(320, window.innerWidth - 24);
    card.style.width = cw + "px";
    if (rect) {
      var roomBelow = window.innerHeight - rect.bottom > 210;
      var top = roomBelow ? rect.bottom + 14 : Math.max(14, rect.top - card.offsetHeight - 14);
      var left = Math.max(12, Math.min(rect.left, window.innerWidth - cw - 12));
      card.style.top = top + "px"; card.style.left = left + "px"; card.style.transform = "none";
    } else {
      card.style.top = "50%"; card.style.left = "50%"; card.style.transform = "translate(-50%,-50%)";
    }
  }
  function onMove() {
    if (!root || track === null) return;
    var t = targetOf(TRACKS[track][idx]);
    var rect = t ? t.getBoundingClientRect() : null;
    placeSpot(rect); positionCard(rect);
  }

  function foot(card, dotCount) {
    var f = elc("div", "tour-foot");
    var dots = elc("div", "tour-dots");
    for (var k = 0; k < dotCount; k++) { var d = elc("span", "tour-dot" + (k === idx ? " on" : "")); dots.appendChild(d); }
    f.appendChild(dots);
    var actions = elc("div", "tour-actions");
    var skip = elc("button", "tour-skip"); skip.type = "button"; skip.textContent = "Skip"; skip.onclick = close;
    actions.appendChild(skip);
    f.appendChild(actions);
    card.appendChild(f);
    return actions;
  }

  function render() {
    var card = root.querySelector(".tour-card");
    card.innerHTML = "";
    if (track === null) {
      placeSpot(null);
      addTitle(card, ROLE.title, ROLE.body);
      var choices = elc("div", "tour-choices");
      choices.appendChild(choice("I'm using my own accounts here", "using"));
      choices.appendChild(choice("I'm sharing an account with others", "admin"));
      choices.appendChild(choice("I'm using one someone shared", "member"));
      card.appendChild(choices);
      foot(card, 0);
      positionCard(null);
      return;
    }
    var steps = TRACKS[track], step = steps[idx], last = idx === steps.length - 1;
    var t = targetOf(step);
    if (t) t.scrollIntoView({ block: "center", behavior: reduce ? "auto" : "smooth" });
    var rect = t ? t.getBoundingClientRect() : null;
    placeSpot(rect);
    addTitle(card, step.title, step.body);
    var actions = foot(card, steps.length);
    if (idx > 0) { var back = elc("button", "tour-back"); back.type = "button"; back.textContent = "Back"; back.onclick = prevStep; actions.appendChild(back); }
    var next = elc("button", "tour-next"); next.type = "button"; next.textContent = last ? "Done" : "Next"; next.onclick = nextStep; actions.appendChild(next);
    positionCard(rect);
  }
  function addTitle(card, title, body) {
    var h = elc("div", "tour-title"); h.textContent = title; card.appendChild(h);
    var b = elc("div", "tour-body"); b.textContent = body; card.appendChild(b);
  }
  function choice(label, which) {
    var c = elc("button", "tour-choice"); c.type = "button"; c.textContent = label;
    c.onclick = function () { track = which; idx = 0; render(); };
    return c;
  }

  function addFab() {
    var fab = elc("button", "help-fab"); fab.type = "button"; fab.textContent = "?";
    fab.title = "Show the intro"; fab.setAttribute("aria-label", "Show the intro");
    fab.onclick = open;
    document.body.appendChild(fab);
  }
  function start() {
    addFab();
    // ?notour=1 skips the first-run intro without marking it seen — the escape
    // hatch for looking at the page itself rather than the welcome over it.
    if (seen() || location.search.includes("notour")) return;
    // First run: open once the claw-slash opening has lifted (intro.js says so),
    // or right away if it already has / never played.
    var go = function () { setTimeout(open, reduce ? 200 : 350); };
    if (document.getElementById("intro")) document.addEventListener("clawdh:introdone", go, { once: true });
    else go();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start);
  else start();
})();
