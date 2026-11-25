// The shell was fetched from a fixed endpoint, but the document still has
// the application's navigation URL. Save it without sending it to the server.
try {
  const path = location.pathname + location.search + location.hash;
  sessionStorage.setItem("__p2page_return_path", JSON.stringify({path, expires:Date.now() + 600000}));
} catch { /* If storage is disabled the login returns to the app root. */ }
location.replace("/__p2page/reauthorize");
