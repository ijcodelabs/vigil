// Applies the theme chosen in the interface to pages outside it (the docs).
try {
  const saved = localStorage.getItem('vigil-theme');
  if (saved) document.documentElement.dataset.theme = saved;
} catch {}
