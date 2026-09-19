import { useEffect, useRef, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";

// The one dialog every decision runs through — a React port of the panel's
// original ask(): a modal with text fields, checkbox groups and copyable code
// boxes, a confirm and (optionally) a cancel. ask(spec) resolves to a map of
// field name -> value, or null if the person backed out. It replaces the
// browser's prompt()/confirm(), which can't do multi-select or a copy button
// and look nothing like the rest of the product.

export type Check = { value: string; label: string; checked?: boolean };
export type Field =
  | { name: string; label: string; placeholder?: string; type?: string }
  | { name: string; label: string; checks: Check[] }
  | { name: string; label: string; pick: { value: string; label: string }[]; value?: string } // one of a few, as a segmented control
  | { name: string; label: string; copy: string };

export interface Spec {
  title: string;
  fields?: Field[];
  note?: string;
  confirm?: string;
  cancel?: string; // "" hides the cancel button (a plain acknowledgement)
  danger?: boolean;
}

export type Answer = Record<string, string | string[]> | null;

const isChecks = (f: Field): f is Extract<Field, { checks: Check[] }> => "checks" in f;
const isPick = (f: Field): f is Extract<Field, { pick: unknown }> => "pick" in f;
const isCopy = (f: Field): f is Extract<Field, { copy: string }> => "copy" in f;

export function useDialog() {
  const [spec, setSpec] = useState<Spec | null>(null);
  const resolver = useRef<((a: Answer) => void) | null>(null);

  const ask = (s: Spec) =>
    new Promise<Answer>((resolve) => {
      resolver.current = resolve;
      setSpec(s);
    });

  const close = (a: Answer) => {
    setSpec(null);
    const r = resolver.current;
    resolver.current = null;
    r?.(a);
  };

  const node = (
    <AnimatePresence>{spec && <DialogView key="dlg" spec={spec} onClose={close} />}</AnimatePresence>
  );
  return { ask, node };
}

function DialogView({ spec, onClose }: { spec: Spec; onClose: (a: Answer) => void }) {
  const fields = spec.fields || [];
  const [text, setText] = useState<Record<string, string>>(() => {
    const t: Record<string, string> = {};
    for (const f of fields) if (isPick(f)) t[f.name] = f.value ?? f.pick[0]?.value ?? ""; else if (!isChecks(f) && !isCopy(f)) t[f.name] = "";
    return t;
  });
  const [checked, setChecked] = useState<Record<string, Set<string>>>(() => {
    const c: Record<string, Set<string>> = {};
    for (const f of fields) if (isChecks(f)) c[f.name] = new Set(f.checks.filter((o) => o.checked).map((o) => o.value));
    return c;
  });
  const first = useRef<HTMLInputElement>(null);
  const showCancel = spec.cancel !== "";
  const cancelLabel = spec.cancel || "Cancel";

  useEffect(() => {
    first.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose(null);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const submit = (e?: React.FormEvent) => {
    e?.preventDefault();
    const out: Record<string, string | string[]> = {};
    for (const f of fields) {
      if (isChecks(f)) out[f.name] = [...(checked[f.name] || [])];
      else if (isCopy(f)) continue;
      else out[f.name] = text[f.name] ?? "";
    }
    onClose(out);
  };

  const toggle = (name: string, value: string) =>
    setChecked((c) => {
      const next = new Set(c[name]);
      next.has(value) ? next.delete(value) : next.add(value);
      return { ...c, [name]: next };
    });

  let firstBound = false;
  return (
    <motion.div
      className="fixed inset-0 z-50 flex items-center justify-center px-4"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.15 }}
      onMouseDown={(e) => e.target === e.currentTarget && onClose(null)}
    >
      <div className="absolute inset-0 bg-black/60 backdrop-blur-[2px]" />
      <motion.form
        onSubmit={submit}
        initial={{ opacity: 0, scale: 0.96, y: 8 }}
        animate={{ opacity: 1, scale: 1, y: 0 }}
        exit={{ opacity: 0, scale: 0.98, y: 6 }}
        transition={{ duration: 0.18, ease: [0.2, 0.8, 0.2, 1] }}
        className="relative w-full max-w-md rounded-2xl border border-line bg-raised p-7 shadow-2xl"
      >
        <h2 className="text-xl font-semibold tracking-tight">{spec.title}</h2>

        <div className="mt-4 flex flex-col gap-4">
          {fields.map((f) => {
            const bindFirst = !firstBound && !isChecks(f) && !isCopy(f) && !isPick(f) ? ((firstBound = true), true) : false;
            return (
              <div key={f.name} className="flex flex-col gap-2">
                <label className="text-[13px] text-faint">{f.label}</label>
                {isCopy(f) ? (
                  <CopyBox text={f.copy} />
                ) : isPick(f) ? (
                  <div className="inline-flex flex-wrap rounded-xl border border-line bg-sunken p-1">
                    {f.pick.map((o) => (
                      <button
                        type="button"
                        key={o.value}
                        onClick={() => setText((t) => ({ ...t, [f.name]: o.value }))}
                        className={`rounded-lg px-3.5 py-1.5 text-[14px] font-medium transition-colors ${text[f.name] === o.value ? "bg-raised-2 text-ink" : "text-muted hover:text-ink"}`}
                      >
                        {o.label}
                      </button>
                    ))}
                  </div>
                ) : isChecks(f) ? (
                  <div className="flex flex-col gap-1.5">
                    {f.checks.map((o) => {
                      const on = (checked[f.name] || new Set()).has(o.value);
                      return (
                        <button
                          type="button"
                          key={o.value}
                          onClick={() => toggle(f.name, o.value)}
                          className={`flex items-center gap-3 rounded-xl border px-3.5 py-3 text-left transition-colors ${
                            on ? "border-primary/60 bg-primary/10" : "border-line bg-sunken hover:border-line/0 hover:bg-raised-2"
                          }`}
                        >
                          <span className={`flex h-5 w-5 shrink-0 items-center justify-center rounded-md border transition-colors ${on ? "border-primary bg-primary text-sunken" : "border-line"}`}>
                            {on && (
                              <svg viewBox="0 0 24 24" className="h-3.5 w-3.5" fill="none" stroke="currentColor" strokeWidth={3}>
                                <path d="M20 6L9 17l-5-5" strokeLinecap="round" strokeLinejoin="round" />
                              </svg>
                            )}
                          </span>
                          <span className="text-[15px] font-medium">{o.label}</span>
                        </button>
                      );
                    })}
                  </div>
                ) : (
                  <input
                    ref={bindFirst ? first : undefined}
                    type={f.type || "text"}
                    value={text[f.name] ?? ""}
                    placeholder={f.placeholder || ""}
                    onChange={(e) => setText((t) => ({ ...t, [f.name]: e.target.value }))}
                    className="w-full rounded-xl border border-line bg-sunken px-4 py-3 text-[15px] outline-none transition-colors focus:border-primary/60"
                  />
                )}
              </div>
            );
          })}
        </div>

        {spec.note && <p className="mt-4 text-[14px] leading-relaxed text-muted">{spec.note}</p>}

        <div className="mt-6 flex justify-end gap-2.5">
          {showCancel && (
            <button
              type="button"
              onClick={() => onClose(null)}
              className="rounded-xl border border-line px-4 py-2 text-[15px] text-muted hover:text-ink"
            >
              {cancelLabel}
            </button>
          )}
          <button
            type="submit"
            className={`rounded-xl px-4 py-2 text-[15px] font-semibold text-sunken transition-transform active:scale-[0.98] ${
              spec.danger ? "bg-crit" : "bg-primary"
            }`}
          >
            {spec.confirm || "OK"}
          </button>
        </div>
      </motion.form>
    </motion.div>
  );
}

// CopyBox is a read-only code line with a Copy button — for invite links.
function CopyBox({ text }: { text: string }) {
  const [label, setLabel] = useState("Copy");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setLabel("Copied");
      setTimeout(() => setLabel("Copy"), 1500);
    } catch {
      setLabel("Copy failed");
    }
  };
  return (
    <div className="flex gap-2">
      <code className="min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded-xl border border-line bg-sunken px-3.5 py-3 font-mono text-[13.5px] text-ink">
        {text}
      </code>
      <button
        type="button"
        onClick={copy}
        className={`shrink-0 rounded-xl border px-4 text-[14px] font-semibold transition-colors ${
          label === "Copied" ? "border-ok bg-ok text-sunken" : "border-line bg-raised-2 text-ink hover:border-primary/50"
        }`}
      >
        {label}
      </button>
    </div>
  );
}
