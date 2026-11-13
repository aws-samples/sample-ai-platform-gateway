// FE-SEC-002: moved out of an inline <script> in console.html so the CSP can drop
// 'unsafe-inline'. Resolves the theme BEFORE the first paint, otherwise the console
// flashes dark and turns light. It only touches the <html> class; wiring the button stays
// in the main script. Order: explicit user choice > OS preference > dark.
(function () {
  try {
    var s = localStorage.getItem('aiplat_theme');
    var t = s || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
    document.documentElement.classList.toggle('dark', t === 'dark');
  } catch (e) {}
})();
