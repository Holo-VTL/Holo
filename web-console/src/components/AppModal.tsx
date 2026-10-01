import { useEffect, useId, useRef, type CSSProperties, type ReactNode } from "react";
import { X } from "lucide-react";
import { useTranslation } from "react-i18next";

type AppModalProps = {
  open: boolean;
  title: string;
  description?: string;
  size?: "medium" | "wide";
  topOffset?: number;
  fixedHeight?: number;
  focusKey?: string;
  busy?: boolean;
  onClose: () => void;
  children: ReactNode;
};

export function AppModal({ open, title, description, size = "medium", topOffset, fixedHeight, focusKey, busy = false, onClose, children }: AppModalProps) {
  const { t } = useTranslation();
  const titleId = useId();
  const descriptionId = useId();
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const titleRef = useRef<HTMLHeadingElement | null>(null);
  const onCloseRef = useRef(onClose);
  const previousFocusKeyRef = useRef(focusKey);

  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);

  useEffect(() => {
    if (!open) {
      previousFocusKeyRef.current = focusKey;
      return;
    }
    if (previousFocusKeyRef.current !== focusKey) titleRef.current?.focus();
    previousFocusKeyRef.current = focusKey;
  }, [focusKey, open]);

  useEffect(() => {
    if (!open) return;
    const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    closeRef.current?.focus();

    function handleKeyDown(event: KeyboardEvent) {
      if (event.key === "Escape" && !busy) {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if (event.key !== "Tab") return;
      const focusable = document.querySelector<HTMLElement>(".app-modal-card")?.querySelectorAll<HTMLElement>(
        'button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (!focusable?.length) {
        event.preventDefault();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    }

    window.addEventListener("keydown", handleKeyDown);
    return () => {
      window.removeEventListener("keydown", handleKeyDown);
      document.body.style.overflow = previousOverflow;
      previousFocus?.focus();
    };
  }, [busy, open]);

  if (!open) return null;

  return (
    <div
      className={`modal-backdrop${topOffset === undefined ? "" : " modal-backdrop-anchored"}`}
      style={topOffset === undefined ? undefined : ({ "--modal-top-offset": `${topOffset}px` } as CSSProperties)}
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose();
      }}
    >
      <section
        className={`modal-card app-modal-card ${size === "wide" ? "app-modal-card-wide" : ""}${topOffset === undefined ? "" : " app-modal-card-anchored"}`}
        style={fixedHeight === undefined ? undefined : { height: `${fixedHeight}px` }}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descriptionId : undefined}
      >
        <header className="app-modal-header">
          <div>
            <h2 id={titleId} ref={titleRef} tabIndex={-1}>{title}</h2>
            {description ? <p id={descriptionId}>{description}</p> : null}
          </div>
          <button ref={closeRef} className="icon-btn" type="button" aria-label={t("common.close")} disabled={busy} onClick={onClose}>
            <X size={17} aria-hidden="true" />
          </button>
        </header>
        <div className="app-modal-body">{children}</div>
      </section>
    </div>
  );
}
