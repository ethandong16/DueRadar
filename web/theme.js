/* Apply the saved theme before first paint; fall back to the system setting. */
(() => {
  let saved = null;
  try { saved = localStorage.getItem("dueradar-theme"); } catch { /* Storage may be blocked. */ }
  let prefersDark = false;
  try { prefersDark = Boolean(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches); } catch { /* Older browsers. */ }
  const dark = saved === "dark" || (!saved && prefersDark);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
})();
