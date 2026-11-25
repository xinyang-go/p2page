// Keep the application's own URL fragment on first sign-in.
if (location.hash) {
  for (const form of document.forms) form.action += location.hash;
  for (const link of document.querySelectorAll("form a")) link.hash = location.hash;
}
