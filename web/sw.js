/* DueRadar app-shell cache. API responses are never cached: tasks and
   credentials stay behind the authenticated request path. Static assets are
   network-first so a deployment is picked up on the next online load; the
   cache is only the offline fallback. */
const CACHE = "dueradar-shell-v2";
const SHELL = ["/", "/style.css", "/theme.js", "/app.js", "/tasks.js", "/manifest.webmanifest", "/icon.svg"];

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((cache) => cache.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))))
      .then(() => self.clients.claim())
  );
});

function networkFirst(request, fallback) {
  const url = new URL(request.url);
  // Store by pathname: hashed ?v= URLs change every deployment, and caching
  // each one under its full URL would grow the shell cache without bound.
  const key = new Request(url.origin + url.pathname);
  return fetch(request)
    .then((response) => {
      if (response.ok) {
        const copy = response.clone();
        caches.open(CACHE).then((cache) => cache.put(key, copy));
      }
      return response;
    })
    .catch(() => caches.match(request, { ignoreSearch: true }).then((cached) => cached || fallback()));
}

self.addEventListener("fetch", (event) => {
  const request = event.request;
  const url = new URL(request.url);
  if (request.method !== "GET" || url.origin !== self.location.origin || url.pathname.startsWith("/api/")) return;
  if (request.mode === "navigate") {
    // Only the dashboard has an app-shell fallback. The slider login page needs
    // a live network and must not silently render the dashboard instead.
    event.respondWith(networkFirst(request, () => url.pathname === "/" ? caches.match("/") : new Response("", { status: 504, statusText: "Offline" })));
    return;
  }
  event.respondWith(networkFirst(request, () => new Response("", { status: 504, statusText: "Offline" })));
});
