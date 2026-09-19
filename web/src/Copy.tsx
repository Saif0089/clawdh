import { useState } from "react";

// copyText puts text on the clipboard, with a fallback for contexts where the
// async clipboard API isn't available (plain-http local pages, older WebKit).
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    try {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand("copy");
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

// CopyButton is the one copy control used everywhere: a small bordered button
// that flips to "Copied" for a moment. `small` is for inline use next to ids.
export function CopyButton({ text, label = "Copy", small = false, className = "" }: { text: string; label?: string; small?: boolean; className?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      title="Copy to clipboard"
      aria-label={`Copy ${label === "Copy" ? "" : label}`.trim()}
      onClick={async (e) => {
        e.stopPropagation();
        if (await copyText(text)) {
          setDone(true);
          setTimeout(() => setDone(false), 1400);
        }
      }}
      className={`shrink-0 rounded-md border bg-raised-2 font-medium transition-colors ${small ? "px-1.5 py-[1px] text-[11px]" : "px-2.5 py-1 text-[12.5px]"} ${
        done ? "border-ok/50 text-ok" : "border-line text-muted hover:border-primary/50 hover:text-ink"
      } ${className}`}
    >
      {done ? "Copied" : label}
    </button>
  );
}

// Mono renders an identifier (a session id, a command) in monospace, truncated
// with a tooltip of the full value, and a copy button beside it.
export function Mono({ text, className = "" }: { text: string; className?: string }) {
  return (
    <span className={`inline-flex min-w-0 items-center gap-1.5 ${className}`}>
      <code title={text} className="min-w-0 truncate rounded bg-sunken px-1.5 py-0.5 font-mono text-[12.5px] text-ink">
        {text}
      </code>
      <CopyButton text={text} small />
    </span>
  );
}
