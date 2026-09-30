// Cache the application shell only. SMS, credentials and APIs never enter CacheStorage.
const CACHE='homesim-shell-v2';const SHELL=['/','/style.css','/fonts.css','/app.js','/ui.js','/theme.js','/icon.svg','/icon-192.png','/icon-512.png','/manifest.webmanifest'];
self.addEventListener('install',e=>e.waitUntil(caches.open(CACHE).then(c=>c.addAll(SHELL))));
self.addEventListener('activate',e=>e.waitUntil(caches.keys().then(keys=>Promise.all(keys.filter(k=>k!==CACHE).map(k=>caches.delete(k))))));
self.addEventListener('fetch',e=>{const u=new URL(e.request.url);if(u.origin!==self.location.origin||e.request.method!=='GET'||u.pathname.startsWith('/api/')||!SHELL.includes(u.pathname))return;e.respondWith(fetch(e.request).catch(()=>caches.match(e.request)));});
