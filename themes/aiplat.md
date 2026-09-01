# Theme: AIPlat

## About

Theme for the AIPlat AI Gateway, derived from **Default (New York)** only in *structure* — the values are the palette that was already in production in the console and the landing page (`domains/frontend/site/app/`). No color was invented: every token below points to a hex that already existed in the inline `tailwind.config` or in the chart configuration.

What this theme solves: previously the palette existed in **four places** (console config, landing config, Chart.js `PALETTE`, and loose literals in each chart's configuration). Changing the brand green left the charts behind. Now there is one `:root`/`.dark` block per page and everything else references it.

Scope: `#111826` is `--card`, not `--background`; green is `--primary` **and** `--chart-1` because in the product green means savings — see Theme UI Rules.

## shadCN CSS Variables

Canonical shadCN contract names, not renamed. **Values in RGB channels** (e.g., `34 197 94`), consumed as `rgb(var(--x))` or `rgb(var(--x) / <alpha>)` — see Deviations.

### Core Variables

| CSS Variable | Light (hex) | Light (RGB) | Dark (hex) | Dark (RGB) | AIPlat token |
|---|---|---|---|---|---|
| `--background` | `#ffffff` | `255 255 255` | `#0b0f17` | `11 15 23` | `ink` |
| `--foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | `text-slate-100` |
| `--card` | `#ffffff` | `255 255 255` | `#111826` | `17 24 38` | `panel` |
| `--card-foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | — |
| `--popover` | `#ffffff` | `255 255 255` | `#111826` | `17 24 38` | `panel` (help drawer) |
| `--popover-foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | — |
| `--primary` | `#16a34a` | `22 163 74` | `#22c55e` | `34 197 94` | `brand` |
| `--primary-foreground` | `#ffffff` | `255 255 255` | `#0b0f17` | `11 15 23` | `ink` (`bg-brand text-ink`) |
| `--secondary` | `#f1f5f9` | `241 245 249` | `#111826` | `17 24 38` | `panel` (`hover:bg-panel`) |
| `--secondary-foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | — |
| `--muted` | `#f1f5f9` | `241 245 249` | `#0e1522` | `14 21 34` | `panel2` |
| `--muted-foreground` | `#475569` | `71 85 105` | `#8ba0b6` | `139 160 182` | `mut` |
| `--accent` | `#f1f5f9` | `241 245 249` | `#0e1522` | `14 21 34` | `panel2` |
| `--accent-foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | — |
| `--destructive` | `#e11d48` | `225 29 72` | `#f43f5e` | `244 63 94` | `danger` |
| `--destructive-foreground` | `#ffffff` | `255 255 255` | `#0b0f17` | `11 15 23` | — |
| `--border` | `#e2e8f0` | `226 232 240` | `#1e2a3a` | `30 42 58` | `line` |
| `--input` | `#e2e8f0` | `226 232 240` | `#1e2a3a` | `30 42 58` | `line` |
| `--ring` | `#0284c7` | `2 132 199` | `#38bdf8` | `56 189 248` | `brand2` |

| CSS Variable | Value | Note |
|---|---|---|
| `--radius` | `0.75rem` | `rounded-xl`, the console cards' radius (New York base uses `0.625rem`) |

### Extensions to the canonical contract

The shadCN contract has no **warning** token (only `destructive`) nor a **switch thumb** token. Both are declared extensions, not renames:

| CSS Variable | Light (hex) | Light (RGB) | Dark (hex) | Dark (RGB) | Use |
|---|---|---|---|---|---|
| `--warning` | `#b45309` | `180 83 9` | `#f59e0b` | `245 158 11` | warning border, background tint, and solid button |
| `--warning-fg` | `#b45309` | `180 83 9` | `#fbbf24` | `251 191 36` | warning **text** |
| `--switch-thumb` | `#ffffff` | `255 255 255` | `#f1f5f9` | `241 245 249` | switch thumb (light in both modes) |

Why `--warning` and `--warning-fg` are separate: the uses have different contrast requirements. `amber-400` as text gives **10.1:1** on the dark card and **~1.7:1** on white — and a warning is exactly what must not go unread. In the light theme both collapse into `#b45309`, which gives **5.0:1** as text on white and accepts near-white text at **4.7:1** when used as a solid background. Solid `amber-500` with light text would give 2.0:1, failing.

`--input` covers two uses in shadCN: field border and the **Switch's off track**. In AIPlat both were already `line`, so the mapping is exact.

### Chart Variables

`--chart-1` through `--chart-5` **are** the five savings mechanisms, in this order — the same as the ROI & Savings tab. This is not a free convention: `RENDER.roi` reads the tokens by index.

| CSS Variable | Mechanism | Light (hex) | Light (RGB) | Dark (hex) | Dark (RGB) |
|---|---|---|---|---|---|
| `--chart-1` | cache (verified) | `#16a34a` | `22 163 74` | `#22c55e` | `34 197 94` |
| `--chart-2` | semantic cache | `#0d9488` | `13 148 136` | `#14b8a6` | `20 184 166` |
| `--chart-3` | auto-cheapest | `#0284c7` | `2 132 199` | `#38bdf8` | `56 189 248` |
| `--chart-4` | fallback | `#d97706` | `217 119 6` | `#f59e0b` | `245 158 11` |
| `--chart-5` | budget degrade | `#7c3aed` | `124 58 237` | `#a78bfa` | `167 139 250` |
| `--chart-6` | — (extension) | `#e11d48` | `225 29 72` | `#f43f5e` | `244 63 94` |
| `--chart-7` | — (extension) | `#ca8a04` | `202 138 4` | `#eab308` | `234 179 8` |
| `--chart-8` | — (extension) | `#6d28d9` | `109 40 217` | `#8b5cf6` | `139 92 246` |

### Sidebar Variables

| CSS Variable | Light (hex) | Light (RGB) | Dark (hex) | Dark (RGB) | Note |
|---|---|---|---|---|---|
| `--sidebar` | `#f8fafc` | `248 250 252` | `#0e1522` | `14 21 34` | the sidebar is `bg-panel2` |
| `--sidebar-foreground` | `#0b0f17` | `11 15 23` | `#f1f5f9` | `241 245 249` | |
| `--sidebar-primary` | `#16a34a` | `22 163 74` | `#22c55e` | `34 197 94` | |
| `--sidebar-primary-foreground` | `#ffffff` | `255 255 255` | `#0b0f17` | `11 15 23` | |
| `--sidebar-accent` | `#f1f5f9` | `241 245 249` | `#111826` | `17 24 38` | active menu item is `bg-panel` |
| `--sidebar-accent-foreground` | `#0b0f17` | `11 15 23` | `#ffffff` | `255 255 255` | …with `text-white` |
| `--sidebar-border` | `#e2e8f0` | `226 232 240` | `#1e2a3a` | `30 42 58` | |
| `--sidebar-ring` | `#0284c7` | `2 132 199` | `#38bdf8` | `56 189 248` | |

### Typography Variables

| CSS Variable | Value |
|---|---|
| `--font-sans` | `ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif` |
| `--font-mono` | `ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace` |

No webfont, by decision: the landing and the console have no build step and no new CDN is allowed, so the system stack is what's available. This differs from New York, which uses Geist.

## Tailwind Mapping

The project uses **Tailwind v3 via CDN**, without `@theme inline`. The AIPlat token names are preserved (thousands of classes depend on them) and now read the canonical variables:

```js
const T = (v) => `rgb(var(${v}) / <alpha-value>)`;
tailwind.config = { theme: { extend: { colors: {
  ink:    T('--background'),        panel:  T('--card'),
  panel2: T('--sidebar'),           line:   T('--border'),
  brand:  T('--primary'),           brand2: T('--ring'),
  mut:    T('--muted-foreground'),  danger: T('--destructive')
}}}}
```

| Class | Variable | Example in use |
|---|---|---|
| `bg-ink` | `--background` | `<body class="bg-ink">` |
| `bg-panel` | `--card` | default card |
| `bg-panel2` | `--sidebar` | sidebar, inputs |
| `border-line` | `--border` | every border and divider |
| `bg-brand` / `text-brand` | `--primary` | primary action, savings |
| `text-brand2` | `--ring` | link, data series, focus |
| `text-mut` | `--muted-foreground` | secondary text |
| `text-danger` | `--destructive` | critical error (console only) |
| `text-fg` | `--foreground` | emphasized text |
| `bg-knob` | `--switch-thumb` | switch thumb |

### Remapped Tailwind scales (important)

The inherited markup carries **named Tailwind colors** hardcoded, which didn't follow the theme and became unreadable in light mode. Instead of editing ~180 occurrences, the scales point to tokens:

| Class in markup | Now reads | Reason |
|---|---|---|
| `text-slate-100`, `text-slate-200` | `--foreground` | were fixed `#f1f5f9` → white on white |
| `text-slate-300` | `--muted-foreground` | same, secondary tone |
| `text-white` → `text-fg` | `--foreground` | replaced in the markup (4 uses) |
| `text-amber-200/300/400` | `--warning-fg` | `#fbbf24` gives ~1.7:1 on white |
| `bg-amber-500`, `border-amber-500` | `--warning` | warning surface and border |

Only the 100/200/300 slate tones and 200/300/400/500 amber tones exist in the file (verified by grep), so the remapping is complete and leaves no remainder.

**Rule that follows from this:** no new screen should use a named Tailwind color (`slate-`, `amber-`, `gray-`, `rose-`…) directly. A hardcoded framework color is silent debt — while the product was dark-only, no one noticed.

`<alpha-value>` is not optional: without it Tailwind doesn't inject alpha and every opacity modifier in use (`bg-brand/10`, `bg-brand/20`, `border-line/60`, `bg-brand2/15`, `bg-ink/80`, `text-mut/70`, `border-danger/40`) stops working **silently**.

## Global Styles

- **Body:** `bg-ink` (`--background`) + `text-slate-100` (`--foreground`)
- **Card:** `bg-panel border border-line rounded-xl p-5` → `--card` / `--border` / `--radius`
- **Sidebar:** `bg-panel2 border-r border-line` → `--sidebar` / `--sidebar-border`
- **Focus:** global rule `:focus-visible{outline:2px solid rgb(var(--ring));outline-offset:2px}` on both pages
- **Scrollbar:** thumb in `rgb(var(--border))`
- **Landing glow:** `radial-gradient(… rgb(var(--primary) / .12) …)`
- **Type scale:** `html{font-size:15px}` **on the console only** (~7% above the Tailwind default)

## Usage Notes

**Charts read the theme.** Chart.js receives color via a helper, not a literal:

```js
const _rootStyle = getComputedStyle(document.documentElement);
function tk(name, alpha){
  const ch = _rootStyle.getPropertyValue(name).trim();
  return alpha == null ? `rgb(${ch})` : `rgb(${ch} / ${alpha})`;
}
```

`getComputedStyle` returns a live object, so `tk()` follows a theme switch at runtime. `Chart.defaults.color` = `--muted-foreground`; `borderColor` = `--border`.

**`PALETTE` preserves the old order** (`--chart-1,3,5,4,6,2,7,8`) on purpose: the doughnut slices per model/feature/team/app have no semantic meaning, and reordering would change their color for no reason. Only change this if you accept the visual change.

**One color literal remains**, deliberately: `ctx.fillStyle='#fff'` on the percentage label drawn *inside* the doughnut slice. It sits over a saturated fill in both modes, so it's a contrast choice, not a theme color — it must not follow `--foreground`.

**Light mode is not enabled.** Both pages pin `<html class="dark">` and there is no toggle. The `:root` block is complete and valid; it exists because the design system requires both modes and for the day a toggle exists. The light values were derived, **not** validated on screen — `--primary` lightens from `#22c55e` to `#16a34a` (green-600) because the dark-mode green doesn't pass contrast as text on white. Before enabling the toggle, the light values need a contrast check.

## Deviations from the base theme structure

Recorded rather than synthesized, so as not to imply a conformance that doesn't exist:

1. **No oklch.** The base tracks oklch (primary) + hex (fallback). Here the values are **hex + RGB channels**, because hex is the real source (the palette already existed in production) and the channels are what Tailwind v3 consumes. Converting to oklch would require recomputing ~60 values; none were estimated.
2. **A variable holds channels, not a full color.** This is the shadCN convention from the Tailwind v3 era (there it was HSL; here RGB, to match the existing hex palette). **Practical consequence:** the CSS in this power's component specs is v4 and writes `background-color: var(--primary)`. Here that must become `background-color: rgb(var(--primary))`. It's not optional — the v4 form does not render.
3. **`--chart-6/7/8` are extensions.** The base defines only `--chart-1..5`. The console doughnuts need 8 categorical series. The three extras are marked as extensions rather than squeezing 8 values into 5 slots.
4. **`--radius` = `0.75rem`**, not `0.625rem`, to match the `rounded-xl` already used on the cards.
5. **No webfont** (Geist) — no build step and no new CDN allowed.

## Theme UI Rules

Rules for this theme. Where they conflict with a general guideline, these prevail.

1. **Green (`--primary` / `--chart-1`) means savings and success.** Never decoration. It's the reason `--primary` and `--chart-1` are the same value. If a design asks for green as an accent, use `--ring` (brand2) or `--muted-foreground`.
2. **Amber (Tailwind `amber-400/500`, outside the canonical contract) is warning and the `preview` state.** It stays a direct Tailwind class, with no dedicated token, because it marks "interface not wired to the backend" and is not a product color.
3. **`--destructive` on the console only.** The landing has no destructive action; the token exists there for contract symmetry.
4. **`--muted-foreground` is the contrast floor for secondary text.** `#8ba0b6` was chosen to pass AA on the `ink` background. Do not darken it.
5. **Color is never the only carrier of information.** Every state also carries a word or icon. This applies to badges, metrics, and table rows.
