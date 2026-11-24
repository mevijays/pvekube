(function () {
  var t = null;
  try { t = localStorage.getItem('kt-theme'); } catch (e) {}
  if (!t) {
    t = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  document.documentElement.setAttribute('data-theme', t);
})();
