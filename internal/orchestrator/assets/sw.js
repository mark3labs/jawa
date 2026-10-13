// The build substitutes a content hash so a new binary gets a fresh cache.
const CACHE = 'jawa-public-c12aca4384be43dd';
const PUBLIC = [
  '/assets/offline.html', '/assets/style.css', '/assets/app.js',
  '/assets/inter.woff2', '/assets/favicon.svg', '/assets/manifest.webmanifest',
  '/assets/icon-180.png', '/assets/icon-192.png', '/assets/icon-512.png',
  '/assets/icon-maskable-512.png',
];
self.addEventListener('install', event => {
  event.waitUntil(caches.open(CACHE).then(cache => cache.addAll(PUBLIC)));
});
self.addEventListener('activate', event => {
  event.waitUntil((async () => {
    for (const key of await caches.keys()) {
      if (key.startsWith('jawa-public-') && key !== CACHE) await caches.delete(key);
    }
    await self.clients.claim();
  })());
});
self.addEventListener('fetch', event => {
  const request = event.request;
  const url = new URL(request.url);
  if (request.method !== 'GET' || url.origin !== self.location.origin) return;
  // Online navigations always go to the server. Never persist session HTML,
  // redirects, streams, API responses, or mutations (even after signing out).
  if (request.mode === 'navigate') {
    event.respondWith(fetch(request).catch(() => caches.match('/assets/offline.html')));
  } else if (!url.search && PUBLIC.includes(url.pathname)) {
    event.respondWith(fetch(request).catch(() => caches.match(request)));
  }
});
