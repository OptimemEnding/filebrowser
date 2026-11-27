const pendingDownloads = new Map();

self.addEventListener("message", (event) => {
  const { id, filename, body } = event.data ?? {};
  const [port] = event.ports;
  if (typeof id !== "string" || !/^[a-f0-9-]{36}$/.test(id) || typeof filename !== "string" ||
      !(body instanceof ReadableStream) || !port) return;

  const encodedName = encodeURIComponent(filename.toWellFormed()).replace(/['()*]/g, (char) =>
    `%${char.charCodeAt(0).toString(16)}`,
  );
  const headers = {
    "Content-Type": "application/octet-stream",
    "Content-Disposition": `attachment; filename*=UTF-8''${encodedName}`,
    "Cache-Control": "no-store",
  };
  const url = new URL(id, self.registration.scope).href;
  const { promise, resolve } = Promise.withResolvers();
  const timeout = setTimeout(() => {
    pendingDownloads.delete(url);
    void body.cancel(new Error("The browser did not start the download")).catch(() => {});
    resolve();
  }, 15000);

  pendingDownloads.set(url, { body, headers, timeout, resolve });
  event.waitUntil(promise);
  port.postMessage(true);
  port.close();
});

self.addEventListener("fetch", (event) => {
  if (event.request.method !== "GET" || event.request.mode !== "navigate") return;
  const download = pendingDownloads.get(event.request.url);
  if (!download) return;

  pendingDownloads.delete(event.request.url);
  clearTimeout(download.timeout);
  event.respondWith(new Response(download.body, { headers: download.headers }));
  download.resolve();
});
