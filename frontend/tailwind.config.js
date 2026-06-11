import forms from "@tailwindcss/forms";

// Tailwind is configured as the consumption layer for the shared
// ZK design-token contract (src/styles/tokens.css). Every colour
// below resolves to an `hsl(var(--zk-*) / <alpha-value>)` expression
// so the token file stays the single source of truth: changing a
// token recolours every utility class and component without touching
// this config. The HSL-channel form (e.g. `221 83% 53%`) is what
// makes the `/<alpha-value>` opacity modifier work (bg-primary/50).
//
// The same token contract is documented in docs/DESIGN_TOKENS.md so
// the zk-drive frontend can adopt an identical :root block and share
// branding across both consoles.
function withVar(name) {
  return `hsl(var(${name}) / <alpha-value>)`;
}

/** @type {import('tailwindcss').Config} */
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    container: {
      center: true,
      padding: "1.5rem",
    },
    extend: {
      colors: {
        background: withVar("--zk-background"),
        foreground: withVar("--zk-foreground"),
        card: {
          DEFAULT: withVar("--zk-card"),
          foreground: withVar("--zk-card-foreground"),
        },
        popover: {
          DEFAULT: withVar("--zk-popover"),
          foreground: withVar("--zk-popover-foreground"),
        },
        primary: {
          DEFAULT: withVar("--zk-primary"),
          foreground: withVar("--zk-primary-foreground"),
        },
        secondary: {
          DEFAULT: withVar("--zk-secondary"),
          foreground: withVar("--zk-secondary-foreground"),
        },
        muted: {
          DEFAULT: withVar("--zk-muted"),
          foreground: withVar("--zk-muted-foreground"),
        },
        accent: {
          DEFAULT: withVar("--zk-accent"),
          foreground: withVar("--zk-accent-foreground"),
        },
        destructive: {
          DEFAULT: withVar("--zk-destructive"),
          foreground: withVar("--zk-destructive-foreground"),
        },
        success: {
          DEFAULT: withVar("--zk-success"),
          foreground: withVar("--zk-success-foreground"),
        },
        warning: {
          DEFAULT: withVar("--zk-warning"),
          foreground: withVar("--zk-warning-foreground"),
        },
        border: withVar("--zk-border"),
        input: withVar("--zk-input"),
        ring: withVar("--zk-ring"),
        chart: {
          1: withVar("--zk-chart-1"),
          2: withVar("--zk-chart-2"),
          3: withVar("--zk-chart-3"),
          4: withVar("--zk-chart-4"),
          5: withVar("--zk-chart-5"),
          6: withVar("--zk-chart-6"),
        },
      },
      borderRadius: {
        lg: "var(--zk-radius)",
        md: "calc(var(--zk-radius) - 2px)",
        sm: "calc(var(--zk-radius) - 4px)",
      },
      fontFamily: {
        sans: "var(--zk-font-sans)",
        mono: "var(--zk-font-mono)",
      },
      boxShadow: {
        card: "var(--zk-shadow-card)",
        overlay: "var(--zk-shadow-overlay)",
      },
      keyframes: {
        "fade-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        "overlay-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        "content-in": {
          from: { opacity: "0", transform: "translate(-50%, -48%) scale(0.97)" },
          to: { opacity: "1", transform: "translate(-50%, -50%) scale(1)" },
        },
        shimmer: {
          "100%": { transform: "translateX(100%)" },
        },
      },
      animation: {
        "fade-in": "fade-in 150ms ease-out",
        "overlay-in": "overlay-in 150ms ease-out",
        "content-in": "content-in 160ms cubic-bezier(0.16, 1, 0.3, 1)",
        shimmer: "shimmer 1.6s infinite",
      },
    },
  },
  plugins: [forms({ strategy: "class" })],
};
