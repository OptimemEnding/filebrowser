import { globalVars } from "@/utils/constants.ts";

export async function prepareBrowserDownload() {
  if (!navigator.serviceWorker || !crypto.randomUUID) return null;

  const workerURL = `${globalVars.baseURL}public/static/chunk-download/worker.js`;
  try {
    const registration = await navigator.serviceWorker.register(workerURL);
    const worker = registration.active ?? registration.installing ?? registration.waiting;
    if (!worker) return null;
    if (worker.state !== "activated") {
      const signal = AbortSignal.timeout(15000);
      await new Promise((resolve, reject) => {
        signal.addEventListener("abort", () => reject(signal.reason), { once: true });
        worker.addEventListener("statechange", () => {
          if (worker.state === "activated" || worker.state === "redundant") resolve();
        }, { signal });
      });
    }
    return worker.state === "activated" ? { worker, scope: registration.scope } : null;
  } catch {
    return null;
  }
}

export async function beginBrowserDownload({ worker, scope }, filename, body, signal) {
  const id = crypto.randomUUID();
  const channel = new MessageChannel();
  const deadline = AbortSignal.any([signal, AbortSignal.timeout(15000)]);
  try {
    deadline.throwIfAborted();
    await new Promise((resolve, reject) => {
      deadline.addEventListener("abort", () => reject(deadline.reason), { once: true });
      channel.port1.onmessage = resolve;
      worker.postMessage({ id, filename, body }, [body, channel.port2]);
    });
  } finally {
    channel.port1.close();
    channel.port2.close();
  }

  signal.throwIfAborted();
  // A navigation lets the download manager consume the worker's attachment response.
  const iframe = document.createElement("iframe");
  iframe.hidden = true;
  iframe.src = new URL(id, scope).href;
  document.body.appendChild(iframe);
  return iframe;
}
