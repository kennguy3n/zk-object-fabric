import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { cn } from "../lib/cn";
import { Card, CardContent } from "../ui/card";
import { Skeleton } from "../ui/skeleton";

interface StatCardProps {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  icon?: LucideIcon;
  // loading swaps the value for a skeleton bar so the card keeps its
  // footprint while the underlying fetch is in flight.
  loading?: boolean;
  // tone tints the icon chip to carry status (e.g. danger for overage).
  tone?: "default" | "success" | "warning" | "destructive";
  className?: string;
}

const TONE_CLASS: Record<NonNullable<StatCardProps["tone"]>, string> = {
  default: "bg-primary/15 text-primary",
  success: "bg-success/15 text-success",
  warning: "bg-warning/15 text-warning",
  destructive: "bg-destructive/15 text-destructive",
};

export function StatCard({
  label,
  value,
  hint,
  icon: Icon,
  loading = false,
  tone = "default",
  className,
}: StatCardProps) {
  return (
    <Card className={className}>
      <CardContent className="flex items-start justify-between gap-3 p-5">
        <div className="min-w-0 space-y-1">
          <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
            {label}
          </div>
          {loading ? (
            <Skeleton className="mt-1 h-8 w-24" />
          ) : (
            <div className="truncate text-2xl font-semibold tabular-nums text-foreground">
              {value}
            </div>
          )}
          {hint && !loading && (
            <div className="text-xs text-muted-foreground">{hint}</div>
          )}
        </div>
        {Icon && (
          <span className={cn("flex size-9 items-center justify-center rounded-lg", TONE_CLASS[tone])}>
            <Icon className="size-5" />
          </span>
        )}
      </CardContent>
    </Card>
  );
}
