const CACHE = "maccellular-remote-v63";
const STATIC = ["./", "app.js?v=20260817-remote39", "session-presentation.js?v=20260815-remote1", "public-voice-ui.js?v=20260816-remote9", "incoming-call-push.js?v=20260815-remote1", "sms-state.js?v=20260814-remote1", "media-client.js?v=20260817-remote27", "call-recorder.js?v=20260817-remote2", "external-voice-client.js?v=20260815-remote2", "style.css?v=20260817-remote8", "manifest.webmanifest", "icon.svg", "icon-192.png", "icon-512.png", "apple-touch-icon.png"];
self.addEventListener("install", (event) => event.waitUntil(
  caches.open(CACHE).then((cache) => cache.addAll(STATIC)).then(() => self.skipWaiting())
));
self.addEventListener("activate", (event) => event.waitUntil((async () => {
  await caches.keys().then((keys) => Promise.all(
    keys.filter((key) => key !== CACHE).map((key) => caches.delete(key))
  ));
  await clients.claim();
})()));
self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith("/api/") || !url.pathname.startsWith("/remote/")) return;
  if (event.request.method !== "GET") return;
  event.respondWith(fetch(event.request).then((response) => {
    if (response.ok && response.type === "basic") {
      const copy = response.clone();
      caches.open(CACHE).then((cache) => cache.put(event.request, copy));
    }
    return response;
  }).catch(() => caches.match(event.request)));
});
self.addEventListener("push", (event) => {
  event.waitUntil((async () => {
    if (!event.data) return;
    let payload;
    try { payload = event.data.json(); } catch { return; }
    if (!payload || payload.version !== 1 || payload.type !== "incoming_call" ||
        Object.keys(payload).length !== 2) return;
    await self.registration.showNotification("MacCellular 来电", {
      body: "打开 MacCellular 查看并处理",
      tag: "maccellular-incoming-call-v1",
      icon: "icon-192.png",
      data: { version: 1 },
    });
  })());
});
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  event.waitUntil(clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (windows) => {
    for (const client of windows) {
      const url = new URL(client.url);
      if (url.origin === self.location.origin && url.pathname.startsWith("/remote/") && "focus" in client) {
        return client.focus();
      }
    }
    return clients.openWindow("/remote/");
  }));
});
