// Apply the server's saved preference before styles or the application load.
(() => {
  const root = document.documentElement;
  const preference = root.dataset.themePreference || 'auto';
  const resolved = preference === 'auto'
    ? window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
    : preference;
  root.dataset.theme = resolved;
  root.style.colorScheme = resolved === 'light' ? 'light' : 'dark';
})();
