import { useId } from "react";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  Cell,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

// The categorical palette resolves to the shared chart tokens
// (--zk-chart-*), so charts recolour with the theme and stay consistent
// with the rest of the console. Values are valid CSS colour expressions
// Recharts passes straight through to SVG fill/stroke.
export const CHART_COLORS = [
  "hsl(var(--zk-chart-1))",
  "hsl(var(--zk-chart-2))",
  "hsl(var(--zk-chart-3))",
  "hsl(var(--zk-chart-4))",
  "hsl(var(--zk-chart-5))",
  "hsl(var(--zk-chart-6))",
];

const AXIS_TICK = { fill: "hsl(var(--zk-muted-foreground))", fontSize: 11 };

export interface ChartDatum {
  label: string;
  value: number;
  // color overrides the palette slot for this datum (e.g. semantic cost rows).
  color?: string;
}

interface TooltipPayloadEntry {
  name?: string | number;
  value?: string | number;
  payload?: ChartDatum & Record<string, unknown>;
  color?: string;
}

// formatFn lets the caller render domain-aware values (bytes, USD) in the
// tooltip without the chart needing to know the unit.
type FormatFn = (value: number) => string;

function TooltipCard({
  active,
  payload,
  label,
  format,
}: {
  active?: boolean;
  payload?: TooltipPayloadEntry[];
  label?: string | number;
  format?: FormatFn;
}) {
  if (!active || !payload || payload.length === 0) return null;
  return (
    <div className="rounded-md border border-border bg-popover px-3 py-2 text-xs shadow-overlay">
      {label != null && label !== "" && (
        <div className="mb-1 font-medium text-popover-foreground">{label}</div>
      )}
      {payload.map((entry, i) => {
        const swatch = entry.payload?.color ?? entry.color;
        const raw = typeof entry.value === "number" ? entry.value : Number(entry.value ?? 0);
        return (
          <div key={i} className="flex items-center gap-2 text-muted-foreground">
            {swatch && (
              <span className="size-2 rounded-full" style={{ background: swatch }} />
            )}
            <span className="text-popover-foreground">
              {entry.payload?.label ?? entry.name}
            </span>
            <span className="ml-auto font-medium tabular-nums text-popover-foreground">
              {format ? format(raw) : raw.toLocaleString()}
            </span>
          </div>
        );
      })}
    </div>
  );
}

// DonutChart renders a proportional breakdown (request mix, cost per
// backend, tier distribution). A centre label surfaces the headline
// total so the donut doubles as a stat.
export function DonutChart({
  data,
  format,
  centerLabel,
  centerValue,
  height = 220,
}: {
  data: ChartDatum[];
  format?: FormatFn;
  centerLabel?: string;
  centerValue?: string;
  height?: number;
}) {
  const total = data.reduce((sum, d) => sum + d.value, 0);
  return (
    <div className="relative" style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        <PieChart>
          <Pie
            data={data}
            dataKey="value"
            nameKey="label"
            innerRadius="62%"
            outerRadius="92%"
            paddingAngle={total > 0 ? 2 : 0}
            stroke="hsl(var(--zk-card))"
            strokeWidth={2}
          >
            {data.map((d, i) => (
              <Cell key={d.label} fill={d.color ?? CHART_COLORS[i % CHART_COLORS.length]} />
            ))}
          </Pie>
          <Tooltip content={<TooltipCard format={format} />} />
        </PieChart>
      </ResponsiveContainer>
      {(centerLabel || centerValue) && (
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center text-center">
          {centerValue && (
            <div className="text-xl font-semibold tabular-nums text-foreground">{centerValue}</div>
          )}
          {centerLabel && (
            <div className="text-xs text-muted-foreground">{centerLabel}</div>
          )}
        </div>
      )}
    </div>
  );
}

// CategoryBarChart renders one bar per category (request counts per verb,
// cost components). Bars use the palette unless a datum sets its own color.
export function CategoryBarChart({
  data,
  format,
  height = 220,
}: {
  data: ChartDatum[];
  format?: FormatFn;
  height?: number;
}) {
  return (
    <div style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        <BarChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 8 }}>
          <XAxis dataKey="label" tick={AXIS_TICK} axisLine={false} tickLine={false} />
          <YAxis
            tick={AXIS_TICK}
            axisLine={false}
            tickLine={false}
            width={44}
            tickFormatter={(v: number) => (format ? format(v) : v.toLocaleString())}
          />
          <Tooltip cursor={{ fill: "hsl(var(--zk-muted) / 0.5)" }} content={<TooltipCard format={format} />} />
          <Bar dataKey="value" radius={[4, 4, 0, 0]} maxBarSize={56}>
            {data.map((d, i) => (
              <Cell key={d.label} fill={d.color ?? CHART_COLORS[i % CHART_COLORS.length]} />
            ))}
          </Bar>
        </BarChart>
      </ResponsiveContainer>
    </div>
  );
}

export interface TrendPoint {
  t: string;
  value: number;
}

// TrendAreaChart plots a live/rolling time series (e.g. requests/min from
// the SSE stream). The gradient fill is theme-aware via the primary token.
export function TrendAreaChart({
  data,
  format,
  height = 200,
}: {
  data: TrendPoint[];
  format?: FormatFn;
  height?: number;
}) {
  const gradientId = useId();
  return (
    <div style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 8, right: 8, bottom: 0, left: 8 }}>
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="hsl(var(--zk-primary))" stopOpacity={0.35} />
              <stop offset="100%" stopColor="hsl(var(--zk-primary))" stopOpacity={0} />
            </linearGradient>
          </defs>
          <XAxis dataKey="t" tick={AXIS_TICK} axisLine={false} tickLine={false} minTickGap={28} />
          <YAxis
            tick={AXIS_TICK}
            axisLine={false}
            tickLine={false}
            width={44}
            tickFormatter={(v: number) => (format ? format(v) : v.toLocaleString())}
          />
          <Tooltip content={<TooltipCard format={format} />} />
          <Area
            type="monotone"
            dataKey="value"
            stroke="hsl(var(--zk-primary))"
            strokeWidth={2}
            fill={`url(#${gradientId})`}
            isAnimationActive={false}
            dot={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}
