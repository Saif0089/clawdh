import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { motion, useReducedMotion } from "framer-motion";

// A small guided intro: a spotlight on a real control plus a short caption, or a
// centered card when a step points at nothing. It drives the panel's own tabs so
// each step lands on the thing it describes, and it degrades gracefully — a
// target that hasn't rendered yet just shows the caption centered. The "?" in the
// corner replays it, and a localStorage flag keeps it from auto-opening twice.

export interface TourStep {
  title: string;
  body: string;
  tab?: string; // switch the panel to this tab before showing the step
  // target: "tab:Accounts" (a nav tab by label), "btn:Add an account" (a button
  // whose text contains this), or a raw CSS selector. Omitted = centered card.
  target?: string;
}

export const introSeenKey = "clawdh:intro:v1";

export function introSeen(): boolean {
  try {
    return localStorage.getItem(introSeenKey) === "1";
  } catch {
    return false;
  }
}
export function markIntroSeen() {
  try {
    localStorage.setItem(introSeenKey, "1");
  } catch {
    /* private mode: the worst case is the intro auto-opens again next time */
  }
}

function findTarget(target?: string): HTMLElement | null {
  if (!target) return null;
  if (target.startsWith("tab:")) {
    const label = target.slice(4);
    return (
      (Array.from(document.querySelectorAll("nav button")) as HTMLElement[]).find(
        (b) => b.textContent?.trim() === label
      ) || null
    );
  }
  if (target.startsWith("btn:")) {
    const label = target.slice(4);
    return (
      (Array.from(document.querySelectorAll("button")) as HTMLElement[]).find((b) =>
        b.textContent?.trim().includes(label)
      ) || null
    );
  }
  return document.querySelector(target);
}

export function Tour({
  steps,
  onClose,
  onTab,
}: {
  steps: TourStep[];
  onClose: () => void;
  onTab?: (tab: string) => void;
}) {
  const reduce = useReducedMotion();
  const [i, setI] = useState(0);
  const [rect, setRect] = useState<DOMRect | null>(null);
  const step = steps[i];

  // Switch to the step's tab, then find + measure its target, retrying while the
  // tab content animates in. No target after a few frames → a centered card.
  useEffect(() => {
    if (!step) return;
    if (step.tab && onTab) onTab(step.tab);
    let raf = 0;
    let tries = 0;
    let scrolled = false;
    const measure = () => {
      const el = findTarget(step.target);
      if (el) {
        if (!scrolled) {
          el.scrollIntoView({ block: "center", behavior: reduce ? "auto" : "smooth" });
          scrolled = true;
        }
        setRect(el.getBoundingClientRect());
      } else if (tries++ < 12) {
        raf = requestAnimationFrame(measure);
        return;
      } else {
        setRect(null);
      }
    };
    raf = requestAnimationFrame(measure);
    return () => cancelAnimationFrame(raf);
  }, [i, step, onTab, reduce]);

  // Keep the spotlight aligned as the page scrolls or resizes.
  useEffect(() => {
    const realign = () => {
      const el = findTarget(step?.target);
      setRect(el ? el.getBoundingClientRect() : null);
    };
    window.addEventListener("resize", realign);
    window.addEventListener("scroll", realign, true);
    return () => {
      window.removeEventListener("resize", realign);
      window.removeEventListener("scroll", realign, true);
    };
  }, [step]);

  // Arrow keys / Escape drive the tour from the keyboard.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
      else if (e.key === "ArrowRight" || e.key === "Enter") setI((n) => Math.min(steps.length - 1, n + 1));
      else if (e.key === "ArrowLeft") setI((n) => Math.max(0, n - 1));
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [steps.length, onClose]);

  if (!step) return null;
  const last = i === steps.length - 1;
  const next = () => (last ? onClose() : setI(i + 1));
  const back = () => setI(Math.max(0, i - 1));

  const cardW = Math.min(320, window.innerWidth - 24);
  const pad = 8;
  let cardStyle: React.CSSProperties;
  if (rect) {
    const roomBelow = window.innerHeight - rect.bottom > 210;
    const top = roomBelow ? rect.bottom + 14 : Math.max(14, rect.top - 200);
    let left = Math.min(rect.left, window.innerWidth - cardW - 12);
    left = Math.max(12, left);
    cardStyle = { position: "fixed", top, left, width: cardW };
  } else {
    cardStyle = { position: "fixed", top: "50%", left: "50%", width: cardW, transform: "translate(-50%,-50%)" };
  }

  const anim = reduce ? {} : { initial: { opacity: 0, y: 6 }, animate: { opacity: 1, y: 0 } };

  return createPortal(
    <div className="fixed inset-0 z-[60]" role="dialog" aria-label="clawdh intro">
      {rect ? (
        <div
          aria-hidden
          style={{
            position: "fixed",
            top: rect.top - pad,
            left: rect.left - pad,
            width: rect.width + pad * 2,
            height: rect.height + pad * 2,
            borderRadius: 12,
            boxShadow: "0 0 0 2px #6E8BFF, 0 0 0 9999px rgba(8,10,18,0.66)",
            transition: reduce ? undefined : "top .25s ease, left .25s ease, width .25s ease, height .25s ease",
            pointerEvents: "none",
          }}
        />
      ) : (
        <div aria-hidden style={{ position: "fixed", inset: 0, background: "rgba(8,10,18,0.66)" }} />
      )}

      <motion.div key={i} {...anim} style={cardStyle} className="rounded-2xl border border-line bg-raised p-4 shadow-2xl">
        <div className="text-[15px] font-semibold text-ink">{step.title}</div>
        <div className="mt-1 text-[13.5px] leading-relaxed text-muted">{step.body}</div>
        <div className="mt-3.5 flex items-center justify-between">
          <div className="flex gap-1.5">
            {steps.map((_, k) => (
              <span key={k} className={`h-1.5 w-1.5 rounded-full transition-colors ${k === i ? "bg-primary" : "bg-line"}`} />
            ))}
          </div>
          <div className="flex items-center gap-2">
            <button onClick={onClose} className="px-1.5 text-[13px] text-faint transition-colors hover:text-muted">
              Skip
            </button>
            {i > 0 && (
              <button onClick={back} className="rounded-lg border border-line px-3 py-1.5 text-[13px] text-muted transition-colors hover:text-ink">
                Back
              </button>
            )}
            <button onClick={next} className="rounded-lg bg-primary px-3.5 py-1.5 text-[13px] font-semibold text-sunken transition-transform active:scale-[0.97]">
              {last ? "Done" : "Next"}
            </button>
          </div>
        </div>
      </motion.div>
    </div>,
    document.body
  );
}

// HelpFab is the "?" in the corner that reopens the intro at any time.
export function HelpFab({ onClick }: { onClick: () => void }) {
  return (
    <button
      onClick={onClick}
      title="Show the intro"
      aria-label="Show the intro"
      className="fixed bottom-3.5 right-3.5 z-40 flex h-9 w-9 items-center justify-center rounded-full border border-line bg-raised text-[17px] font-semibold text-muted opacity-85 shadow-lg transition-colors hover:border-primary/60 hover:text-ink sm:bottom-5 sm:right-5 sm:h-11 sm:w-11 sm:text-[19px] sm:opacity-100"
    >
      ?
    </button>
  );
}

// adminSteps is the panel's guided tour, for the admin who lends accounts out.
export const adminSteps: TourStep[] = [
  {
    title: "Welcome to your clawdh panel",
    body: "This is where you lend your team Claude logins — one login, many people, handed out and taken back through the gateway. A quick tour:",
  },
  {
    tab: "people",
    target: "btn:Invite someone",
    title: "1 · Invite your people",
    body: "Send an invite link. One click on it joins their computer — or installs clawdh and joins — and they’re set up. No login ever leaves the gateway.",
  },
  {
    tab: "accounts",
    target: "btn:How do I add one?",
    title: "2 · Logins arrive from machines",
    body: "Anyone who has joined adds a Claude login from their own clawdh page, in one click. It lands here; “Give access” on its card lends it to someone.",
  },
  {
    tab: "usage",
    target: "tab:Usage",
    title: "3 · Watch usage",
    body: "One card per account: its real 5-hour and weekly windows, and who ran it on which models. Green, amber and red only ever mean how full a window is.",
  },
  {
    tab: "quotas",
    target: "tab:Quotas",
    title: "4 · Set a ceiling",
    body: "Stop a person, or everyone on an account, once its weekly window is fuller than a percent you pick — a reserve that can’t be eaten into.",
  },
  {
    title: "Use it on your own machine",
    body: "Install clawdh, then open http://127.0.0.1:47932 (or run “clawdh status” for the exact URL). The ? in the corner replays this anytime.",
  },
];

// gateSteps runs before sign-in (and for a visitor who was shared with): what
// clawdh is, and how a member gets going.
export const gateSteps: TourStep[] = [
  {
    title: "Welcome to clawdh",
    body: "clawdh lends a team’s Claude logins through one gateway — shared safely, taken back anytime. Sign in to manage yours.",
  },
  {
    title: "Just joining?",
    body: "Someone shared with you: install clawdh, open your machine’s page, and paste your invite under “Got an invite?”. Then run “clawdh shared <name>”.",
  },
];
