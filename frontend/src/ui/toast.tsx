import * as ToastPrimitive from "@radix-ui/react-toast";
import { CheckCircle2, Info, TriangleAlert, X } from "lucide-react";
import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { cn } from "../lib/cn";

type ToastVariant = "default" | "success" | "destructive";

interface ToastItem {
  id: number;
  title: string;
  description?: string;
  variant: ToastVariant;
}

interface ToastApi {
  // toast enqueues a transient notification. Returns nothing; toasts
  // auto-dismiss and can be swipe/closed manually.
  toast(input: { title: string; description?: string; variant?: ToastVariant }): void;
}

const ToastContext = createContext<ToastApi | null>(null);

const ICONS: Record<ToastVariant, ReactNode> = {
  default: <Info className="size-4 text-primary" />,
  success: <CheckCircle2 className="size-4 text-success" />,
  destructive: <TriangleAlert className="size-4 text-destructive" />,
};

let nextId = 1;

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);

  const remove = useCallback((id: number) => {
    setItems((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const toast = useCallback<ToastApi["toast"]>(({ title, description, variant = "default" }) => {
    setItems((prev) => [...prev, { id: nextId++, title, description, variant }]);
  }, []);

  const api = useMemo(() => ({ toast }), [toast]);

  return (
    <ToastContext.Provider value={api}>
      <ToastPrimitive.Provider swipeDirection="right" duration={5000}>
        {children}
        {items.map((t) => (
          <ToastPrimitive.Root
            key={t.id}
            onOpenChange={(open) => !open && remove(t.id)}
            className={cn(
              "group pointer-events-auto relative flex w-full items-start gap-3 rounded-md border border-border bg-card p-4 pr-8 shadow-overlay",
              "data-[state=open]:animate-fade-in data-[swipe=end]:translate-x-[var(--radix-toast-swipe-end-x)]",
            )}
          >
            <span className="mt-0.5 shrink-0">{ICONS[t.variant]}</span>
            <div className="flex flex-col gap-0.5">
              <ToastPrimitive.Title className="text-sm font-medium text-foreground">
                {t.title}
              </ToastPrimitive.Title>
              {t.description && (
                <ToastPrimitive.Description className="text-xs text-muted-foreground">
                  {t.description}
                </ToastPrimitive.Description>
              )}
            </div>
            <ToastPrimitive.Close className="absolute right-2 top-2 rounded-sm text-muted-foreground opacity-70 transition-opacity hover:opacity-100">
              <X className="size-4" />
            </ToastPrimitive.Close>
          </ToastPrimitive.Root>
        ))}
        <ToastPrimitive.Viewport className="fixed bottom-0 right-0 z-[100] flex w-full max-w-sm flex-col gap-2 p-4 outline-none" />
      </ToastPrimitive.Provider>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastApi {
  const ctx = useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used inside <ToastProvider>");
  }
  return ctx;
}
