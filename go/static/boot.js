// Early boot shims. Loaded synchronously from <head> before the app modules:
// apply the saved theme and hide the login form while a stored session is
// being restored. Kept in a file (not inline) so the CSP can require
// script-src 'self' without hashes.
(function () {
  try {
    var t = localStorage.getItem("eneverre.theme");
    if (t === "light" || t === "dark") {
      document.documentElement.dataset.theme = t;
    }
  } catch (e) {}
})();

(function () {
  try {
    if (localStorage.getItem("eneverre.token")) {
      document.documentElement.classList.add("booting");
    }
  } catch (e) {}
})();
