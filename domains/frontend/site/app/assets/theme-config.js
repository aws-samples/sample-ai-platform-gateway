// FE-SEC-002: moved out of an inline <script> in console.html so the CSP can drop
// 'unsafe-inline'. Same-origin (script-src 'self') and runs in document order, right
// after vendor/tailwind so tailwind.config is set before Tailwind processes the page.
//
// The AIPlat token names (ink/panel/brand/…) stay the same — thousands of classes in the
// HTML depend on them. What changes is the SOURCE of the value: it comes from the
// canonical design-system variables declared in assets/console.css, instead of hex
// repeated here. One palette, one place.
//
// The `rgb(var(--x) / <alpha-value>)` form is mandatory: without it Tailwind cannot inject
// alpha and every opacity modifier in use (bg-brand/10, border-line/60, bg-brand2/15,
// text-mut/70…) would silently stop working. That is also why the variables hold RGB
// CHANNELS and not hex — the same convention shadCN uses for Tailwind v3.
// IIFE so a one-letter global does not leak into the page's shared scope.
(function () {
  const T = (v) => `rgb(var(${v}) / <alpha-value>)`;
  tailwind.config = { theme: { extend: { colors: {
    ink:    T('--background'),        panel:  T('--card'),
    panel2: T('--sidebar'),           line:   T('--border'),
    brand:  T('--primary'),           brand2: T('--ring'),
    mut:    T('--muted-foreground'),  danger: T('--destructive'),
    fg:     T('--foreground'),        knob:   T('--switch-thumb'),
    // slate PALETTE REMAPPING. The markup carries ~20 uses of text-slate-100/200/300 — a
    // HARDCODED Tailwind color that did not follow the theme. In light mode that painted
    // almost-white text on white: the metric numbers, the view title, the sidebar
    // "AIPlat" and the active menu item simply disappeared. Instead of editing every
    // occurrence, the slate scale itself now points at tokens. Only the 100/200/300
    // shades exist in the file (verified), so the remapping is complete.
    slate:  { 100: T('--foreground'), 200: T('--foreground'), 300: T('--muted-foreground') },
    // amber PALETTE REMAPPING — same problem as slate, 156 uses. amber-400 (#fbbf24) on
    // the dark card gives 10.1:1, excellent; on white it gives ~1.7:1, illegible. A
    // warning is precisely what the user must NOT fail to read. Two tokens because the
    // uses differ: 200/300/400 are TEXT (they darken in the light theme) and 500 is
    // surface/border (bg-amber-500/10, border-amber-500/40 and the solid Save button).
    amber:  { 200: T('--warning-fg'), 300: T('--warning-fg'),
              400: T('--warning-fg'), 500: T('--warning') }
  }}}};
})();
