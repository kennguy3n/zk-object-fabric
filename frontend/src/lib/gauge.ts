// gaugeColor maps a ratio in [0, 1] to a semantic token colour for the
// SVG GaugeChart, keeping the higher-is-better vs higher-is-worse
// policy in the page layer (the gauge component stays presentation-only).
//
// - "higher-better" (e.g. cache hit ratio): green when healthy, amber
//   when soft, red when poor.
// - "lower-better" (e.g. budget consumed): green with headroom, amber
//   when approaching the cap, red at/over the cap.
export type GaugeSemantics = "higher-better" | "lower-better";

const SUCCESS = "hsl(var(--zk-success))";
const WARNING = "hsl(var(--zk-warning))";
const DESTRUCTIVE = "hsl(var(--zk-destructive))";

export function gaugeColor(ratio: number, semantics: GaugeSemantics): string {
  const v = Number.isFinite(ratio) ? ratio : 0;
  if (semantics === "higher-better") {
    if (v >= 0.9) return SUCCESS;
    if (v >= 0.75) return WARNING;
    return DESTRUCTIVE;
  }
  if (v >= 0.95) return DESTRUCTIVE;
  if (v >= 0.8) return WARNING;
  return SUCCESS;
}
