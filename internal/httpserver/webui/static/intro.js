// intro.js — the claw-slash opening's small moving parts. The show itself is
// CSS (style.css, "the claw-slash opening"): this adds the sparks that fly off
// each slash tip, removes the overlay once it has lifted, and tells the rest of
// the page (the first-run tour waits for it) with a "clawdh:introdone" event.
// Under prefers-reduced-motion the overlay is hidden by CSS and removed here at
// once, so nothing waits on it.
(function () {
  "use strict";

  var el = document.getElementById("intro");
  if (!el) return;

  var reduce = false;
  try { reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches; } catch (e) { /* old browser */ }

  function done() {
    if (!el) return;
    el.remove();
    el = null;
    document.documentElement.classList.add("intro-done");
    document.dispatchEvent(new CustomEvent("clawdh:introdone"));
  }
  if (reduce) { done(); return; }

  // Sparks: seven off each tip, fanned along the slash's exit direction. The
  // offsets come from a tiny seeded generator, so the show is identical every
  // load. Timings match the CSS: slash i lands at .52s + i·.11s.
  var seed = 7;
  function rnd() { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; }
  var svgns = "http://www.w3.org/2000/svg";
  var holder = document.getElementById("intro-sparks");
  for (var i = 0; i < 3; i++) {
    var tipX = 196 + i * 60, tipY = 262, landing = 0.52 + i * 0.11;
    for (var k = 0; k < 7; k++) {
      var angle = Math.atan2(86, 28) + (rnd() - 0.5) * 1.9;
      var dist = 34 + rnd() * 64;
      var c = document.createElementNS(svgns, "circle");
      c.setAttribute("class", "intro-spark");
      c.setAttribute("cx", tipX);
      c.setAttribute("cy", tipY);
      c.setAttribute("r", (1.6 + rnd() * 2.2).toFixed(2));
      c.style.fill = k % 3 === 0 ? "#fff" : "var(--primary-2)";
      c.style.setProperty("--dx", (Math.cos(angle) * dist).toFixed(1) + "px");
      c.style.setProperty("--dy", (Math.sin(angle) * dist).toFixed(1) + "px");
      c.style.animationDelay = (landing - 0.08 + rnd() * 0.06).toFixed(2) + "s";
      holder.appendChild(c);
    }
  }

  el.addEventListener("animationend", function (e) { if (e.target === el) done(); });
  setTimeout(done, 2600); // a throttled background tab may never fire animationend
})();
