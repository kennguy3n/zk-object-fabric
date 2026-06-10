import { cn } from "../lib/cn";

// Skeleton is a content-shaped placeholder shown while data loads. The
// shimmer keeps it visibly "alive" so an operator can tell the page is
// fetching rather than stalled.
export function Skeleton({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn(
        "relative overflow-hidden rounded-md bg-muted skeleton-shimmer",
        className,
      )}
      {...props}
    />
  );
}
