import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";

type ToastLevel = "success" | "error" | "info";

type ToastMessage = {
  id: number;
  level: ToastLevel;
  text: string;
};

type ToastContextValue = {
  push: (text: string, level?: ToastLevel) => void;
};

const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [messages, setMessages] = useState<ToastMessage[]>([]);

  const push = useCallback((text: string, level: ToastLevel = "info") => {
    const id = Date.now() + Math.floor(Math.random() * 1000);
    setMessages((prev) => [...prev, { id, level, text }]);
    window.setTimeout(() => {
      setMessages((prev) => prev.filter((msg) => msg.id !== id));
    }, 3200);
  }, []);

  const value = useMemo(() => ({ push }), [push]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div className="toast-host" aria-live="polite">
        {messages.map((msg) => (
          <div key={msg.id} className={`toast toast-${msg.level}`}>
            {msg.text}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used within ToastProvider");
  }
  return ctx;
}
