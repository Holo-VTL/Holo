import { Check, ChevronDown } from "lucide-react";
import { KeyboardEvent, useEffect, useId, useRef, useState } from "react";

export type SelectInputOption = {
  value: string;
  label: string;
  disabled?: boolean;
};

type SelectInputProps = {
  value: string;
  options: SelectInputOption[];
  onChange: (value: string) => void;
  ariaLabel?: string;
  disabled?: boolean;
  required?: boolean;
};

export function SelectInput({ value, options, onChange, ariaLabel, disabled = false, required = false }: SelectInputProps) {
  const listboxId = useId();
  const rootRef = useRef<HTMLDivElement | null>(null);
  const buttonRef = useRef<HTMLButtonElement | null>(null);
  const [open, setOpen] = useState(false);
  const selectedIndex = Math.max(0, options.findIndex((option) => option.value === value));
  const selected = options[selectedIndex];

  useEffect(() => {
    if (!open) {
      return;
    }
    function closeOnOutsideClick(event: MouseEvent) {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false);
      }
    }
    document.addEventListener("mousedown", closeOnOutsideClick);
    return () => document.removeEventListener("mousedown", closeOnOutsideClick);
  }, [open]);

  function selectOption(option: SelectInputOption) {
    if (option.disabled) {
      return;
    }
    onChange(option.value);
    setOpen(false);
    buttonRef.current?.focus();
  }

  function moveSelection(direction: 1 | -1) {
    if (options.length === 0) {
      return;
    }
    let next = selectedIndex;
    for (let checked = 0; checked < options.length; checked += 1) {
      next = (next + direction + options.length) % options.length;
      if (!options[next].disabled) {
        selectOption(options[next]);
        return;
      }
    }
  }

  function handleKeyDown(event: KeyboardEvent<HTMLButtonElement>) {
    if (event.key === "ArrowDown") {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        return;
      }
      moveSelection(1);
    }
    if (event.key === "ArrowUp") {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        return;
      }
      moveSelection(-1);
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      setOpen((prev) => !prev);
    }
    if (event.key === "Escape") {
      setOpen(false);
    }
  }

  return (
    <div className="select-input" ref={rootRef}>
      <button
        ref={buttonRef}
        className="input select-input-trigger"
        type="button"
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={listboxId}
        aria-required={required}
        disabled={disabled}
        onClick={() => setOpen((prev) => !prev)}
        onKeyDown={handleKeyDown}
      >
        <span className={!selected?.value ? "select-input-placeholder" : undefined}>{selected?.label || ""}</span>
        <ChevronDown size={16} aria-hidden="true" />
      </button>
      {open ? (
        <div className="select-input-menu" id={listboxId} role="listbox" aria-label={ariaLabel}>
          {options.map((option) => (
            <button
              className={`select-input-option ${option.value === value ? "select-input-option-selected" : ""}`}
              key={option.value || option.label}
              type="button"
              role="option"
              aria-selected={option.value === value}
              disabled={option.disabled}
              onClick={() => selectOption(option)}
            >
              <span className="select-input-check">{option.value === value ? <Check size={16} aria-hidden="true" /> : null}</span>
              <span>{option.label}</span>
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
