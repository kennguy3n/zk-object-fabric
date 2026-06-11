import { CircleAlert, Inbox, RefreshCw, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { cn } from "../lib/cn";
import { Button } from "../ui/button";
import { Card } from "../ui/card";

// EmptyState communicates "nothing here yet" with an optional call to
// action, so a fetched-but-empty list never looks like a failed load.
export function EmptyState({
  icon: Icon = Inbox,
  title,
  description,
  action,
  className,
}: {
  icon?: LucideIcon;
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-3 px-6 py-12 text-center",
        className,
      )}
    >
      <span className="flex size-12 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <Icon className="size-6" />
      </span>
      <div className="space-y-1">
        <div className="text-sm font-medium text-foreground">{title}</div>
        {description && (
          <div className="mx-auto max-w-sm text-sm text-muted-foreground">{description}</div>
        )}
      </div>
      {action}
    </div>
  );
}

// ErrorState renders a fetch/mutation failure with an optional retry. It
// is intentionally calm (no full-page red) so a single failing panel
// does not make the whole console look broken.
export function ErrorState({
  title = "Something went wrong",
  message,
  onRetry,
  className,
}: {
  title?: string;
  message: string;
  onRetry?: () => void;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-3 px-6 py-10 text-center",
        className,
      )}
    >
      <span className="flex size-12 items-center justify-center rounded-full bg-destructive/15 text-destructive">
        <CircleAlert className="size-6" />
      </span>
      <div className="space-y-1">
        <div className="text-sm font-medium text-foreground">{title}</div>
        <div className="mx-auto max-w-md break-words text-sm text-muted-foreground">{message}</div>
      </div>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RefreshCw className="size-4" /> Retry
        </Button>
      )}
    </div>
  );
}

// InlineError is a compact banner for form/mutation errors that should
// sit next to the control that triggered them.
export function InlineError({ message, className }: { message: string; className?: string }) {
  return (
    <div
      role="alert"
      className={cn(
        "flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive",
        className,
      )}
    >
      <CircleAlert className="mt-0.5 size-4 shrink-0" />
      <span className="break-words">{message}</span>
    </div>
  );
}

// CardError wraps ErrorState in a Card for use as a full-panel fallback.
export function CardError(props: Parameters<typeof ErrorState>[0]) {
  return (
    <Card>
      <ErrorState {...props} />
    </Card>
  );
}
