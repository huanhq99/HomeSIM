(() => {
  const media = matchMedia('(prefers-color-scheme: dark)');
  let preference = 'system';
  try { preference = localStorage.getItem('homesim-theme') || 'system'; } catch {}
  if (!['system', 'light', 'dark'].includes(preference)) preference = 'system';
  function apply(value) {
    preference = value;
    const dark = value === 'dark' || (value === 'system' && media.matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    document.querySelector('meta[name="theme-color"]').content = dark ? '#141c18' : '#f5f7f4';
  }
  window.HomeTheme = { get: () => preference, set(value) { apply(value); try { localStorage.setItem('homesim-theme', value); } catch {} } };
  media.addEventListener('change', () => apply(preference));
  apply(preference);
})();
