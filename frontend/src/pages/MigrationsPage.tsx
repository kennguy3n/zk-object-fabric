import { ArrowRight, CheckCircle2, CircleAlert, Clock, Loader2, Replace } from "lucide-react";
import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { MigrationJob, MigrationJobState } from "../api/types";
import { PageHeader } from "../components/PageHeader";
import { CardError, EmptyState } from "../components/states";
import { cn } from "../lib/cn";
import { formatBytes, formatNumber, formatRelative } from "../format";
import { Badge } from "../ui/badge";
import { Card, CardContent } from "../ui/card";
import { Progress } from "../ui/progress";
import { Skeleton } from "../ui/skeleton";

const STATES: MigrationJobState[] = ["pending", "running", "done"];

const STATE_BADGE: Record<MigrationJobState, { variant: "neutral" | "default" | "success" | "destructive"; label: string }> = {
  pending: { variant: "neutral", label: "Pending" },
  running: { variant: "default", label: "Running" },
  done: { variant: "success", label: "Done" },
  failed: { variant: "destructive", label: "Failed" },
};

// MigrationsPage renders fleet migration jobs (GET /api/v1/migrations).
// The orchestrator does not expose a total-bytes target, so progress is
// shown as a state-machine stepper plus live throughput / copied
// counters rather than a fake percentage. Running jobs poll every 5s.
export function MigrationsPage() {
  const [jobs, setJobs] = useState<MigrationJob[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      setJobs(await api.listMigrations());
      setError(null);
    } catch (err) {
      if (!silent) setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (!silent) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // Poll while any job is in flight so progress stays live without a
  // websocket. Stops once everything is terminal.
  const hasActive = jobs.some((j) => j.state === "pending" || j.state === "running");
  useEffect(() => {
    if (!hasActive) return;
    const id = setInterval(() => void refresh(true), 5000);
    return () => clearInterval(id);
  }, [hasActive, refresh]);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Migrations"
        description="Fleet migration jobs moving buckets between backends and dedicated cells. Active jobs refresh automatically."
      />

      {loading ? (
        <div className="space-y-4">{[0, 1].map((i) => <Skeleton key={i} className="h-40 w-full" />)}</div>
      ) : error ? (
        <CardError message={error} onRetry={() => void refresh()} />
      ) : jobs.length === 0 ? (
        <Card>
          <CardContent className="p-0">
            <EmptyState icon={Replace} title="No migrations" description="Backend or cell migrations triggered by operations will appear here with live progress." />
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-4">
          {jobs.map((job) => (
            <JobCard key={job.job_id} job={job} />
          ))}
        </div>
      )}
    </div>
  );
}

function JobCard({ job }: { job: MigrationJob }) {
  const badge = STATE_BADGE[job.state];
  return (
    <Card>
      <CardContent className="space-y-4 p-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <span className="font-medium text-foreground">{job.bucket}</span>
              <Badge variant={badge.variant}>{badge.label}</Badge>
            </div>
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <span className="font-mono text-xs">{job.source_backend}</span>
              <ArrowRight className="size-3.5" />
              <span className="font-mono text-xs">{job.dest_backend}</span>
              <span className="text-xs">· cell {job.dest_cell_id}</span>
            </div>
          </div>
          <div className="text-right text-xs text-muted-foreground">
            <div className="font-mono">{job.job_id}</div>
            {job.started_at && <div>started {formatRelative(job.started_at)}</div>}
            {job.completed_at && <div>completed {formatRelative(job.completed_at)}</div>}
          </div>
        </div>

        <StateStepper state={job.state} />

        <div className="grid grid-cols-2 gap-4 sm:grid-cols-3">
          <Metric label="Copied" value={formatBytes(job.bytes_copied)} />
          <Metric label="Pieces" value={formatNumber(job.pieces_copied)} />
          <Metric label="Throughput" value={job.state === "running" ? `${formatBytes(job.bytes_per_second)}/s` : "—"} />
        </div>

        {job.state === "running" && <Progress value={null} aria-label="Migration in progress" />}
        {job.state === "failed" && job.error && (
          <div className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            <CircleAlert className="mt-0.5 size-4 shrink-0" />
            <span className="break-words">{job.error}</span>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="text-sm font-medium tabular-nums text-foreground">{value}</div>
    </div>
  );
}

// StateStepper visualises the pending → running → done state machine.
// A failed job marks the running step as errored.
function StateStepper({ state }: { state: MigrationJobState }) {
  const failed = state === "failed";
  const activeIndex = failed ? 1 : STATES.indexOf(state);
  return (
    <div className="flex items-center">
      {STATES.map((s, i) => {
        const done = i < activeIndex || state === "done";
        const current = i === activeIndex && !done;
        const errored = failed && i === 1;
        return (
          <div key={s} className="flex flex-1 items-center last:flex-none">
            <div className="flex items-center gap-2">
              <span
                className={cn(
                  "flex size-7 items-center justify-center rounded-full border text-xs",
                  errored
                    ? "border-destructive bg-destructive/15 text-destructive"
                    : done
                      ? "border-success bg-success/15 text-success"
                      : current
                        ? "border-primary bg-primary/15 text-primary"
                        : "border-border bg-muted text-muted-foreground",
                )}
              >
                {errored ? <CircleAlert className="size-4" /> : done ? <CheckCircle2 className="size-4" /> : current ? <Loader2 className="size-4 animate-spin" /> : <Clock className="size-4" />}
              </span>
              <span className={cn("text-xs capitalize", done || current || errored ? "text-foreground" : "text-muted-foreground")}>
                {errored ? "failed" : s}
              </span>
            </div>
            {i < STATES.length - 1 && <div className={cn("mx-3 h-px flex-1", done ? "bg-success" : "bg-border")} />}
          </div>
        );
      })}
    </div>
  );
}
