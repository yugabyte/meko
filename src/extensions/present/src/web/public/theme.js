// Meko's light or dark choice (its web UI keeps it under meko.theme on the same origin), else the
// system's, applied before first paint.
(function () {
  var t = null;
  try {
    t = localStorage.getItem('meko.theme');
  } catch {
    /* no storage */
  }
  if (t !== 'light' && t !== 'dark') t = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  document.documentElement.classList.toggle('dark', t === 'dark');
})();
