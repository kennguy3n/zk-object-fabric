// GaugeChart renders a dependency-free SVG semicircle gauge for a
// single ratio in [0, 1] — cache hit ratio, egress budget consumed,
// cache warm-up progress, etc. The component is intentionally
// policy-free: the caller supplies the fill colour (and an optional
// display string) so the same gauge can express "higher is better"
// (hit ratio) and "higher is worse" (egress budget) semantics without
// the component knowing which is which. This mirrors the rest of the
// console, where presentation lives in the page and the shared piece
// stays generic.

interface GaugeChartProps {
  // value is the ratio to plot, clamped to [0, 1].
  value: number;
  // label is the caption rendered under the gauge (e.g. "Cache hit ratio").
  label?: string;
  // valueLabel overrides the centred readout. Defaults to the value
  // formatted as a whole-number percentage (e.g. "90%").
  valueLabel?: string;
  // color is the arc fill. Defaults to the accent colour.
  color?: string;
  // size is the gauge width in pixels. Height is ~60% of width.
  size?: number;
}

// clamp keeps the plotted value inside [0, 1] so a transient
// over-budget (>100%) reading does not draw the arc past the track.
function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0;
  if (v < 0) return 0;
  if (v > 1) return 1;
  return v;
}

export function GaugeChart({
  value,
  label,
  valueLabel,
  color = "var(--accent)",
  size = 160,
}: GaugeChartProps) {
  const v = clamp01(value);
  // Geometry: a 180° arc from left (180°) to right (0°). The stroke
  // is drawn on a semicircle of radius r centred at (cx, cy); we use
  // stroke-dasharray over the arc length to reveal `v` of it.
  const stroke = 14;
  const w = size;
  const r = (w - stroke) / 2;
  const cx = w / 2;
  const cy = w / 2;
  const arcLength = Math.PI * r; // half circumference
  const dash = `${arcLength * v} ${arcLength}`;
  // viewBox height only needs the top half plus stroke + room for the
  // centred readout.
  const h = cy + stroke;
  const readout = valueLabel ?? `${Math.round(v * 100)}%`;
  // Describe the semicircle path: start at the left end, sweep over
  // the top to the right end.
  const arc = `M ${stroke / 2} ${cy} A ${r} ${r} 0 0 1 ${w - stroke / 2} ${cy}`;

  return (
    <div style={{ display: "flex", flexDirection: "column", alignItems: "center", gap: 4 }}>
      <svg width={w} height={h} viewBox={`0 0 ${w} ${h}`} role="img" aria-label={label ?? readout}>
        {/* Track */}
        <path
          d={arc}
          fill="none"
          stroke="var(--border)"
          strokeWidth={stroke}
          strokeLinecap="round"
        />
        {/* Value arc */}
        <path
          d={arc}
          fill="none"
          stroke={color}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={dash}
        />
        <text
          x={cx}
          y={cy - 6}
          textAnchor="middle"
          fontSize={28}
          fontWeight={700}
          fill="var(--text)"
        >
          {readout}
        </text>
      </svg>
      {label && (
        <div className="muted" style={{ fontSize: 12, textTransform: "uppercase", letterSpacing: "0.04em" }}>
          {label}
        </div>
      )}
    </div>
  );
}
