import { useEffect, useMemo, useRef } from "react";
import { useReducedMotion } from "framer-motion";

// The claw-slash: clawdh's opening. Three slashes rake across the dark, each
// with a soft glow, a hot white core and a spray of sparks off its tip; the
// third strike lands with a flash and a ring, and the wordmark sharpens out of
// the blur behind them with the surface's badge under it. Then the overlay
// lifts to reveal the app. ~1.8 s, once per load — the caller unmounts it.
//
// The show itself is CSS (index.css, "the claw-slash opening"), the same
// markup and keyframes the local "This machine" page uses (intro.js /
// style.css) in that surface's teal — one choreography, two accents. This
// component only lays out the markup, seeds the sparks and reports when the
// overlay has lifted. Under prefers-reduced-motion nothing is drawn and it
// resolves at once.

// Sparks: seven off each tip, fanned along the slash's exit direction. The
// offsets come from a tiny seeded generator, so the show is identical every
// load and there's nothing random to make a screenshot flake. Timings match
// the CSS: slash i lands at .52s + i·.11s.
type Spark = { x: number; y: number; dx: number; dy: number; r: number; delay: number; white: boolean };
function sparks(): Spark[] {
  let seed = 7;
  const rnd = () => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff);
  const out: Spark[] = [];
  for (let i = 0; i < 3; i++) {
    const tipX = 196 + i * 60, tipY = 262, landing = 0.52 + i * 0.11;
    for (let k = 0; k < 7; k++) {
      const angle = Math.atan2(86, 28) + (rnd() - 0.5) * 1.9;
      const dist = 34 + rnd() * 64;
      out.push({
        x: tipX, y: tipY,
        dx: Math.cos(angle) * dist, dy: Math.sin(angle) * dist,
        r: 1.6 + rnd() * 2.2,
        delay: landing - 0.08 + rnd() * 0.06,
        white: k % 3 === 0,
      });
    }
  }
  return out;
}

const slash = (i: number) => `M ${70 + i * 60} 34 C ${150 + i * 60} 120, ${168 + i * 60} 176, ${196 + i * 60} 262`;

export function ClawIntro({ onDone, badge = "Team panel" }: { onDone: () => void; badge?: string }) {
  const reduce = useReducedMotion();
  const sparkList = useMemo(sparks, []);
  const done = useRef(false);
  const finish = () => {
    if (done.current) return;
    done.current = true;
    onDone();
  };

  useEffect(() => {
    if (reduce) finish();
    // A throttled background tab may never fire animationend.
    const t = setTimeout(finish, 2600);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reduce]);

  if (reduce) return null;

  return (
    <div
      className="intro"
      aria-hidden
      onAnimationEnd={(e) => {
        if (e.target === e.currentTarget && e.animationName === "intro-lift") finish();
      }}
    >
      <div className="intro-stage">
        <svg className="intro-svg" viewBox="0 0 420 300">
          <defs>
            <linearGradient id="intrograd" x1="0" y1="0" x2="420" y2="300" gradientUnits="userSpaceOnUse">
              <stop offset="0" className="intro-stop-a" />
              <stop offset="1" className="intro-stop-b" />
            </linearGradient>
            <radialGradient id="introflash">
              <stop offset="0" className="intro-stop-a" stopOpacity="0.75" />
              <stop offset="0.55" className="intro-stop-b" stopOpacity="0.18" />
              <stop offset="1" className="intro-stop-b" stopOpacity="0" />
            </radialGradient>
            <filter id="introglow" x="-50%" y="-50%" width="200%" height="200%">
              <feGaussianBlur stdDeviation="7" />
            </filter>
          </defs>
          <circle className="intro-flash" cx="210" cy="150" r="190" fill="url(#introflash)" />
          <circle className="intro-ring" cx="210" cy="150" r="200" />
          {[0, 1, 2].map((i) => (
            <g key={i} className={`intro-slash s${i}`}>
              <path className="glow" pathLength={1} d={slash(i)} />
              <path className="core" pathLength={1} d={slash(i)} />
              <path className="hot" pathLength={1} d={slash(i)} />
            </g>
          ))}
          <g>
            {sparkList.map((s, k) => (
              <circle
                key={k}
                className="intro-spark"
                cx={s.x}
                cy={s.y}
                r={s.r.toFixed(2)}
                style={{
                  fill: s.white ? "#fff" : "var(--primary-2)",
                  animationDelay: `${s.delay.toFixed(2)}s`,
                  ["--dx" as string]: `${s.dx.toFixed(1)}px`,
                  ["--dy" as string]: `${s.dy.toFixed(1)}px`,
                }}
              />
            ))}
          </g>
        </svg>
        <div className="intro-word">
          clawdh<span className="intro-pill">{badge}</span>
        </div>
      </div>
    </div>
  );
}
