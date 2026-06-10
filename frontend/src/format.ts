// formatBytes converts a byte count into a human-readable string.
// Binary prefixes (KiB, MiB, …) match how the gateway reports
// storage in `billing/metering.go`.
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const exponent = Math.min(Math.floor(Math.log2(bytes) / 10), units.length - 1);
  const value = bytes / 2 ** (exponent * 10);
  const precision = value >= 100 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(precision)} ${units[exponent]}`;
}

// formatUSD renders a dollar amount with cents. Used across the Billing
// screen so every cost row shares one currency convention.
export function formatUSD(value: number): string {
  if (!Number.isFinite(value)) return "—";
  return value.toLocaleString("en-US", {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: 2,
  });
}

// formatNumber renders an integer-ish count with thousands separators.
export function formatNumber(value: number): string {
  if (!Number.isFinite(value)) return "—";
  return Math.round(value).toLocaleString("en-US");
}

// formatPercent renders a [0,1] ratio as a whole-number percentage.
export function formatPercent(ratio: number, digits = 0): string {
  if (!Number.isFinite(ratio)) return "—";
  return `${(ratio * 100).toFixed(digits)}%`;
}

// formatDateTime renders an ISO timestamp in the operator's locale,
// returning a dash for the empty/zero values the backend omits.
export function formatDateTime(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1) return "—";
  return d.toLocaleString();
}

// formatRelative renders a compact "time ago" string for recent
// activity (e.g. key last-used). Falls back to absolute for old dates.
export function formatRelative(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1) return "—";
  const seconds = Math.round((Date.now() - d.getTime()) / 1000);
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days}d ago`;
  return d.toLocaleDateString();
}
