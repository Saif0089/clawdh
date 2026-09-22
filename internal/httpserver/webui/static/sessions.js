// sessions.js — where new work starts, and what is running right now.
//
// The two things this page could not previously answer for anyone using an
// editor. Running an account was a command on a card, which is no use to
// someone who never opens a terminal; moving a conversation you were already
// in was a phrase you had to have been told. Both are controls now, and they
// mean the same thing in an editor as in a terminal.
//
// It leans on app.js for `api`, `copyToClipboard`, `currentAccounts`,
// `currentShares` and `loginsByDir` — one page, one set of facts, so the
// account list here can never disagree with the cards below.
"use strict";

(function () {
  const startAccount = document.getElementById("start-account");
  const startPicker = document.getElementById("start-picker");
  const startApply = document.getElementById("start-apply");
  const startProblem = document.getElementById("start-problem");
  const startResult = document.getElementById("start-result");
  const laneCmd = document.getElementById("lane-cmd-text");
  const laneCopy = document.getElementById("lane-cmd-copy");
  const editorState = document.getElementById("editor-state");
  const editorList = document.getElementById("editor-list");
  const liveBlock = document.getElementById("live-block");
  const liveList = document.getElementById("live-list");
  const liveEmpty = document.getElementById("live-empty");
  const liveResult = document.getElementById("live-result");
  const liveAll = document.getElementById("live-all");
  const liveAllPicker = document.getElementById("live-all-picker");
  const liveAllApply = document.getElementById("live-all-apply");
  const liveRowTemplate = document.getElementById("live-row-template");

  // The last snapshot, so a click knows what it is acting on without asking
  // the server again for something it was just told.
  let snapshot = { sessions: [], default: { set: false }, editors: [], supported: false };

  // --- targets ---------------------------------------------------------

  // An account a session can run as: one of this machine's logins, or an
  // account shared with it. A local account with no login on this machine is
  // left out — picking it would start a session that can do nothing.
  function targets() {
    const out = [];
    for (const a of (typeof currentAccounts !== "undefined" ? currentAccounts : [])) {
      if (!loginsByDir[a.configDir || ""]) continue;
      out.push({ key: "a:" + a.id, label: a.name, accountId: a.id });
    }
    for (const sh of (typeof currentShares !== "undefined" ? currentShares : [])) {
      out.push({ key: "s:" + sh.slug, label: sh.account + " (shared)", shared: sh.slug });
    }
    return out;
  }

  function targetByKey(key) {
    return targets().find((t) => t.key === key) || null;
  }

  // body is what the two switch endpoints take: one account, named the way
  // whichever kind it is has to be named.
  function body(target, extra) {
    const b = Object.assign({}, extra || {});
    if (target.shared) b.shared = target.shared;
    else b.accountId = target.accountId;
    return b;
  }

  // fillPicker rebuilds a dropdown only when its options actually changed, so
  // a poll every few seconds never yanks a half-made choice out from under
  // someone. The chosen value is kept when it still exists.
  function fillPicker(select, list, selectedKey) {
    const want = JSON.stringify(list.map((t) => [t.key, t.label]));
    if (select.dataset.options !== want) {
      const had = select.value;
      select.replaceChildren();
      for (const t of list) {
        const opt = document.createElement("option");
        opt.value = t.key;
        opt.textContent = t.label;
        select.appendChild(opt);
      }
      select.dataset.options = want;
      if (had && list.some((t) => t.key === had)) select.value = had;
      else if (selectedKey) select.value = selectedKey;
    } else if (selectedKey && !select.dataset.touched) {
      select.value = selectedKey;
    }
    select.disabled = list.length === 0;
  }

  function say(el, text, kind) {
    if (!text) { el.hidden = true; return; }
    el.hidden = false;
    el.textContent = text;
    el.className = "start-result" + (kind ? " " + kind : "");
  }

  function plural(n, one, many) { return n === 1 ? one : many; }

  // --- rendering -------------------------------------------------------

  function render(data) {
    snapshot = data;
    const list = targets();

    // Where new sessions start.
    const def = data.default || { set: false };
    startAccount.textContent = def.set && def.label ? def.label : "your default login";
    startAccount.classList.toggle("unset", !def.set);
    const key = def.shared ? "s:" + def.shared : def.accountId ? "a:" + def.accountId : "";
    fillPicker(startPicker, list, key);
    startApply.disabled = list.length === 0;
    if (startProblem) {
      startProblem.hidden = !def.problem;
      startProblem.textContent = def.problem || "";
    }

    // The terminal lane names a real account rather than a placeholder, so the
    // command on it can be copied and run as it stands.
    const first = list.find((t) => !t.shared) || list[0];
    if (first) {
      laneCmd.textContent = first.shared ? "clawdh shared " + first.shared : "clawdh " + slugFor(first.accountId);
      laneCopy.hidden = false;
    } else {
      laneCmd.textContent = "clawdh <name>";
      laneCopy.hidden = true;
    }

    renderEditors(data.editors || []);
    renderLive(data.sessions || [], list, data.supported);
  }

  function slugFor(accountId) {
    const a = (typeof currentAccounts !== "undefined" ? currentAccounts : []).find((x) => x.id === accountId);
    return a ? a.slug : "<name>";
  }

  function renderEditors(list) {
    const withExt = list.filter((e) => e.hasExtension);
    const managed = withExt.filter((e) => e.managed);
    editorList.replaceChildren();

    if (list.length === 0) {
      editorState.textContent = "No editor found";
      editorState.className = "lane-state";
      return;
    }
    if (withExt.length === 0) {
      editorState.textContent = "Claude Code extension not installed";
      editorState.className = "lane-state";
      return;
    }
    if (managed.length === withExt.length) {
      editorState.textContent = "Ready";
      editorState.className = "lane-state ok";
    } else {
      editorState.textContent = "Not set up";
      editorState.className = "lane-state warn";
      const fix = document.createElement("button");
      fix.type = "button";
      fix.className = "editor-fix";
      fix.textContent = "Set up";
      fix.addEventListener("click", () => setUpEditors(fix));
      editorList.appendChild(fix);
    }
    for (const ed of list) {
      const row = document.createElement("span");
      row.className = "editor-chip" + (ed.managed ? " on" : ed.hasExtension ? " off" : " na");
      row.textContent = ed.name;
      row.title = ed.managed
        ? "Chats here run through clawdh."
        : ed.hasExtension
          ? "The Claude Code extension is here, but clawdh isn't in its launch path yet."
          : "No Claude Code extension installed in this editor.";
      editorList.appendChild(row);
    }
  }

  function renderLive(live, list, supported) {
    liveList.replaceChildren();
    liveEmpty.hidden = live.length > 0;
    if (!supported) {
      liveEmpty.hidden = false;
      liveEmpty.textContent = "Can't read this machine's sessions.";
    }
    liveAll.hidden = live.length < 2 || list.length === 0;
    if (!liveAll.hidden) fillPicker(liveAllPicker, list, "");

    for (const s of live) {
      const node = liveRowTemplate.content.cloneNode(true);
      const row = node.querySelector(".live-row");
      row.dataset.pid = s.pid;

      const host = node.querySelector(".live-host");
      host.textContent = s.host === "editor" ? (s.editor || "Editor") : "Terminal";
      host.className = "live-host " + (s.host === "editor" ? "editor" : "terminal");
      node.querySelector(".live-dir").textContent = s.where || "";

      const select = node.querySelector(".live-account");
      const here = s.shared ? "s:" + s.slug : "a:" + s.accountId;
      const options = list.slice();
      // A session can be running as something that is no longer offered — a
      // share taken back, a login signed out. Name it anyway: the row has to
      // say what this session is actually on.
      if (!options.some((t) => t.key === here)) options.unshift({ key: here, label: s.account });
      fillPicker(select, options, here);
      select.value = here;
      select.addEventListener("change", () => moveOne(row, select, s));
      liveList.appendChild(node);
    }
  }

  // --- actions ---------------------------------------------------------

  async function setUpEditors(btn) {
    btn.disabled = true;
    const was = btn.textContent;
    btn.textContent = "Setting up…";
    try {
      render(await api("/api/editors/setup", { method: "POST" }));
    } catch (e) {
      say(startResult, "Could not set the editors up: " + e.message, "bad");
    } finally {
      btn.disabled = false;
      btn.textContent = was;
    }
  }

  // "Use for everything": new sessions start here, and everything already
  // running moves over. One button, because that is one intention.
  startApply.addEventListener("click", async () => {
    const target = targetByKey(startPicker.value);
    if (!target) return;
    const live = snapshot.sessions || [];
    const moving = live.filter((s) => !onTarget(s, target));
    if (moving.length) {
      const ok = confirm(
        `Start new sessions as ${target.label}, and move the ${moving.length} ${plural(moving.length, "session", "sessions")} running now?\n\n` +
        `Each one restarts on ${target.label} with its conversation resumed. Anything running inside them at that moment — a subagent, a background task — does not survive.`);
      if (!ok) return;
    }
    startApply.disabled = true;
    const was = startApply.textContent;
    startApply.textContent = "Applying…";
    try {
      render(await api("/api/sessions/default", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body(target)),
      }));
      let moved = 0;
      if (moving.length) {
        const res = await api("/api/sessions/switch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body(target, { all: true })),
        });
        moved = (res && res.moved) || 0;
      }
      say(startResult, moved
        ? `New sessions start as ${target.label}. ${moved} ${plural(moved, "session is", "sessions are")} moving over now.`
        : `New sessions start as ${target.label}.`, "ok");
      startPicker.dataset.touched = "";
      refreshSessions();
    } catch (e) {
      say(startResult, e.message, "bad");
    } finally {
      startApply.disabled = false;
      startApply.textContent = was;
    }
  });

  startPicker.addEventListener("change", () => { startPicker.dataset.touched = "1"; });

  liveAllApply.addEventListener("click", async () => {
    const target = targetByKey(liveAllPicker.value);
    if (!target) return;
    const moving = (snapshot.sessions || []).filter((s) => !onTarget(s, target));
    if (!moving.length) { say(liveResult, `Everything is already on ${target.label}.`, "ok"); return; }
    const ok = confirm(
      `Move ${moving.length} running ${plural(moving.length, "session", "sessions")} to ${target.label}?\n\n` +
      `Each one restarts there with its conversation resumed. Anything running inside them right now does not survive. New sessions are unaffected.`);
    if (!ok) return;
    liveAllApply.disabled = true;
    try {
      const res = await api("/api/sessions/switch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body(target, { all: true })),
      });
      say(liveResult, `${res.moved} ${plural(res.moved, "session is", "sessions are")} moving to ${target.label}.`, "ok");
      refreshSessions();
    } catch (e) {
      say(liveResult, e.message, "bad");
    } finally {
      liveAllApply.disabled = false;
    }
  });

  // Moving one session needs no confirmation: the dropdown already says what
  // it is doing, the conversation survives, and changing it back is the same
  // one click. The caveat about what does not survive is under the list.
  async function moveOne(row, select, live) {
    const target = targetByKey(select.value);
    if (!target || onTarget(live, target)) return;
    const state = row.querySelector(".live-state");
    select.disabled = true;
    state.hidden = false;
    state.textContent = "moving…";
    state.className = "live-state";
    try {
      await api("/api/sessions/switch", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body(target, { pid: live.pid })),
      });
      state.textContent = "restarting on " + target.label;
      refreshSessions();
    } catch (e) {
      state.textContent = e.message;
      state.className = "live-state bad";
      select.value = live.shared ? "s:" + live.slug : "a:" + live.accountId;
    } finally {
      select.disabled = false;
    }
  }

  function onTarget(live, target) {
    return target.shared ? (live.shared && live.slug === target.shared) : (!live.shared && live.accountId === target.accountId);
  }

  laneCopy.addEventListener("click", () => copyToClipboard(laneCmd.textContent, laneCopy));

  // The caveat about what a move costs is worth knowing once, not worth a
  // paragraph on the page for ever after.
  const liveHelp = document.getElementById("live-help");
  const liveNote = document.getElementById("live-note");
  liveHelp.addEventListener("click", () => {
    const show = liveNote.hidden;
    liveNote.hidden = !show;
    liveHelp.setAttribute("aria-expanded", String(show));
  });

  // --- polling ---------------------------------------------------------

  // Called by app.js's poll, so the whole page is one read of one machine.
  window.refreshSessions = async function refreshSessions() {
    try {
      render(await api("/api/sessions"));
    } catch (_) {
      // The rest of the page is still true; a register that cannot be read is
      // not a reason to blank the accounts above it.
    }
  };
})();
