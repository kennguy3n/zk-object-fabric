# ZK Design Tokens — shared brand contract

This document defines the **design-token contract** shared by the two ZK
frontends:

- `zk-object-fabric` — the tenant/operator **console** (`frontend/`), which
  consumes the tokens through Tailwind.
- `zk-drive` — the consumer/SME **drive** app, which can either `@import` the
  same token file or mirror the `:root` / `.dark` blocks verbatim.

The single source of truth lives in this repo at
[`frontend/src/styles/tokens.css`](../frontend/src/styles/tokens.css). Treat
token **names** as a stable, versioned API: add new tokens rather than renaming
or repurposing existing ones, so a token change in one app never silently
breaks the other.

## How it fits together

```
tokens.css            tailwind.config.js              components
(:root / .dark)  ->   colors.* = hsl(var(--zk-*)  ->  className="bg-primary
HSL channels          / <alpha-value>)               text-muted-foreground"
```

1. `tokens.css` declares each colour as **HSL channels only** — `H S% L%`
   (e.g. `221 83% 53%`), not a finished `hsl(...)` string.
2. `tailwind.config.js` maps every semantic colour to
   `hsl(var(--zk-token) / <alpha-value>)`. Storing channels (rather than a
   complete colour) is what lets Tailwind apply opacity modifiers such as
   `bg-primary/40` or `border-border/60`.
3. Components only ever reference the **semantic Tailwind class**
   (`bg-card`, `text-foreground`, `ring-ring`, …) — never a raw hex or a raw
   `--zk-*` variable — so re-theming is a token-file edit with zero component
   churn.

Non-Tailwind consumers (plain CSS, inline styles, charts) wrap a token in
`hsl()` directly:

```css
.badge { color: hsl(var(--zk-foreground)); background: hsl(var(--zk-muted)); }
```

```ts
// Recharts series read the categorical palette off the CSS variables so a
// chart recolours with the theme (see frontend/src/components/charts.tsx).
const stroke = `hsl(var(--zk-chart-1))`;
```

## Theming

- `:root` is the **light** theme (zk-drive's default surface).
- `.dark` is the **operator-console** theme (zk-object-fabric mounts the app
  under `class="dark"`).

Switching themes is a single class toggle on a wrapping element; no JS token
recomputation is required because both blocks define the full token set.

## Token reference

Every token is prefixed `--zk-` to avoid collisions when the file is imported
into an app that already defines its own custom properties.

### Surfaces

| Token | Tailwind class | Purpose |
|---|---|---|
| `--zk-background` / `--zk-foreground` | `bg-background` / `text-foreground` | Page surface and default text. |
| `--zk-card` / `--zk-card-foreground` | `bg-card` / `text-card-foreground` | Raised panels, tables, cards. |
| `--zk-popover` / `--zk-popover-foreground` | `bg-popover` / `text-popover-foreground` | Floating surfaces (menus, dialogs, tooltips). |

### Brand & interactive

| Token | Tailwind class | Purpose |
|---|---|---|
| `--zk-primary` / `--zk-primary-foreground` | `bg-primary` / `text-primary-foreground` | Primary actions, active nav, focus accents. |
| `--zk-secondary` / `--zk-secondary-foreground` | `bg-secondary` / `text-secondary-foreground` | Secondary buttons and fills. |
| `--zk-accent` / `--zk-accent-foreground` | `bg-accent` / `text-accent-foreground` | Highlights and secondary emphasis. |
| `--zk-muted` / `--zk-muted-foreground` | `bg-muted` / `text-muted-foreground` | Subdued fills and helper text. |

### Semantic status

| Token | Tailwind class | Purpose |
|---|---|---|
| `--zk-success` / `--zk-success-foreground` | `bg-success` / `text-success-foreground` | Healthy state, headroom, success toasts. |
| `--zk-warning` / `--zk-warning-foreground` | `bg-warning` / `text-warning-foreground` | Approaching-limit / caution state. |
| `--zk-destructive` / `--zk-destructive-foreground` | `bg-destructive` / `text-destructive-foreground` | Errors, over-cap, destructive actions. |

The gauge semantics helper (`frontend/src/lib/gauge.ts`) maps a ratio to these
three status tokens so utilisation/budget gauges colour consistently across the
console (`higher-better` vs `lower-better`).

### Lines & focus

| Token | Tailwind class | Purpose |
|---|---|---|
| `--zk-border` | `border-border` | Hairline borders and dividers. |
| `--zk-input` | `border-input` | Form-control borders. |
| `--zk-ring` | `ring-ring` | Keyboard-focus ring. |

### Categorical chart palette

`--zk-chart-1` … `--zk-chart-6` (Tailwind `text-chart-1` / `fill-chart-1`, etc.)
are an ordered, colour-blind-aware series palette for provider / cost / usage
breakdowns. Always allocate series in order starting at `--zk-chart-1` so the
same dimension keeps the same colour across charts and across both apps.

### Shape, type & elevation

| Token | Tailwind binding | Purpose |
|---|---|---|
| `--zk-radius` | `rounded-sm/md/lg` | Corner-radius scale (`lg` = base, `md`/`sm` derived). |
| `--zk-font-sans` | `font-sans` | UI typeface (Inter, with system fallbacks). |
| `--zk-font-mono` | `font-mono` | Monospace (keys, IDs, policy JSON). |
| `--zk-shadow-card` | `shadow-card` | Card elevation. |
| `--zk-shadow-overlay` | `shadow-overlay` | Dialog / popover elevation. |

## Adopting in zk-drive

1. Copy `frontend/src/styles/tokens.css` into the zk-drive frontend (or import
   it from a shared package) and ensure it is imported **before** Tailwind's
   base layer.
2. Reuse the `withVar()` colour mapping from this repo's
   [`tailwind.config.js`](../frontend/tailwind.config.js) so the same semantic
   classes resolve identically.
3. Build UI against the semantic classes only. Do not hard-code hex values or
   read `--zk-*` variables directly in components except for charts/inline
   styles that cannot use a utility class.

## Change policy

- **Additive only** for names: new needs get a new `--zk-*` token.
- A token's **value** may change (re-theme), but its **semantic meaning** must
  not (e.g. `--zk-destructive` always means "error/danger").
- Keep `:root` and `.dark` in lockstep — every token defined in one must be
  defined in the other.
- When you add or change a token, update this document in the same change so
  the contract stays the source of truth both apps review against.
