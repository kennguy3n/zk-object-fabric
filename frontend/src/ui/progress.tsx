import * as ProgressPrimitive from "@radix-ui/react-progress";
import { forwardRef } from "react";

import { cn } from "../lib/cn";

export interface ProgressProps
  extends React.ComponentPropsWithoutRef<typeof ProgressPrimitive.Root> {
  // value is a percentage in [0, 100]. Pass null for an indeterminate bar.
  value?: number | null;
  indicatorClassName?: string;
}

export const Progress = forwardRef<
  React.ElementRef<typeof ProgressPrimitive.Root>,
  ProgressProps
>(({ className, value, indicatorClassName, ...props }, ref) => {
  const pct = value == null ? null : Math.min(100, Math.max(0, value));
  return (
    <ProgressPrimitive.Root
      ref={ref}
      value={pct ?? undefined}
      className={cn("relative h-2 w-full overflow-hidden rounded-full bg-muted", className)}
      {...props}
    >
      <ProgressPrimitive.Indicator
        className={cn(
          "h-full w-full flex-1 rounded-full bg-primary transition-all",
          pct == null && "animate-pulse",
          indicatorClassName,
        )}
        style={{ transform: `translateX(-${100 - (pct ?? 40)}%)` }}
      />
    </ProgressPrimitive.Root>
  );
});
Progress.displayName = ProgressPrimitive.Root.displayName;
