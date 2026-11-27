import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/utils/constants", () => ({
  globalVars: { baseURL: "/", externalUrl: "" },
}));

vi.mock("@/utils/streamDownload.js", () => ({
  prepareBrowserDownload: vi.fn(),
  beginBrowserDownload: vi.fn(),
}));

vi.mock("@/store", () => ({
  getters: { isShare: () => false },
  mutations: { showPrompt: vi.fn() },
  state: {
    sessionId: "test-session",
    shareInfo: { hash: "different-share" },
    user: { fileLoading: { downloadChunkSizeMb: 1 } },
    prompts: [],
  },
}));

import { download } from "@/api/resources.js";
import { mutations } from "@/store/index.ts";
import { downloadManager } from "@/utils/downloadManager.ts";
import { beginBrowserDownload, prepareBrowserDownload } from "@/utils/streamDownload.js";

afterEach(() => {
  downloadManager.reset();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.clearAllMocks();
  localStorage.clear();
});

const preflightOK = { ok: true, status: 200, headers: new Headers({ "X-Download-ID": "tracked-id" }) };
const transferComplete = (bytes, total) => ({ ok: true, json: async () => ({ status: "completed", bytes, total }) });
const folder = { type: "directory", path: "/backup", source: "files" };

describe("chunked file download", () => {
  const chunkSize = 1024 * 1024;
  const file = { type: "file", name: "backup.bin", path: "/backup.bin", source: "files", size: 4 * chunkSize + 7 };
  function mockBrowserStream(write = vi.fn()) {
    const remove = vi.fn();
    prepareBrowserDownload.mockResolvedValueOnce({});
    beginBrowserDownload.mockImplementationOnce(async (_delivery, _name, stream) => {
      void stream.pipeTo(new WritableStream({ write })).catch(() => {});
      return { remove };
    });
    return remove;
  }

  it("writes each range of a protected share before requesting the next range", async () => {
    localStorage.setItem("sharepass:requested-share", "passé");
    const data = Buffer.alloc(file.size);
    for (let offset = 0; offset < data.length; offset += chunkSize) {
      data.fill(offset / chunkSize + 1, offset, Math.min(offset + chunkSize, data.length));
    }
    const received = [];
    let written = 0;
    const remove = mockBrowserStream((bytes) => {
      received.push(Buffer.from(bytes));
      written += bytes.byteLength;
    });
    const fetchMock = vi.fn(async (_url, options) => {
      const [, start, end] = options.headers.Range.match(/^bytes=(\d+)-(\d+)$/).map(Number);
      expect(written).toBe(start);
      expect(remove).not.toHaveBeenCalled();
      if (start > 0) expect(options.headers["If-Range"]).toBe('"version-1"');
      const body = new ReadableStream({
        start(controller) {
          for (let offset = start; offset <= end; offset += 65536) {
            controller.enqueue(data.subarray(offset, Math.min(offset + 65536, end + 1)));
          }
          controller.close();
        },
      });
      return new Response(body, {
        status: 206,
        headers: { "Content-Range": `bytes ${start}-${end}/${data.length}`, ETag: '"version-1"' },
      });
    });
    vi.stubGlobal("fetch", fetchMock);
    vi.stubGlobal("Uint8Array", new Proxy(Uint8Array, {
      construct(target, args) {
        if (typeof args[0] === "number" && args[0] >= chunkSize) {
          throw new RangeError("Array buffer allocation failed");
        }
        return Reflect.construct(target, args);
      },
    }));

    await download(null, [file], "requested-share");

    expect(beginBrowserDownload).toHaveBeenCalledWith({}, "backup.bin", expect.any(ReadableStream), expect.any(AbortSignal));
    expect(fetchMock.mock.calls.map(([, options]) => options.headers.Range)).toEqual([
      "bytes=0-1048575", "bytes=1048576-2097151", "bytes=2097152-3145727",
      "bytes=3145728-4194303", "bytes=4194304-4194310",
    ]);
    expect(fetchMock.mock.calls.every(([, options]) => options.headers["X-SHARE-PASSWORD"] === "pass%C3%A9")).toBe(true);
    expect(remove).toHaveBeenCalledOnce();
    expect(Buffer.concat(received).equals(data)).toBe(true);
    expect(downloadManager.queue[0]).toMatchObject({ name: "backup.bin", status: "completed", loaded: file.size, progress: 100, abortController: null });
  });

  it("writes the entire first response when the server ignores Range and omits Content-Length", async () => {
    const data = Buffer.alloc(file.size, 7);
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response(data));
    vi.stubGlobal("fetch", fetchMock);
    const received = [];
    const remove = mockBrowserStream((bytes) => received.push(Buffer.from(bytes)));

    await download(null, [file]);

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(remove).toHaveBeenCalledOnce();
    expect(Buffer.concat(received).equals(data)).toBe(true);
    expect(downloadManager.queue[0]).toMatchObject({ status: "completed", loaded: file.size });
  });

  it("discards a short range even when its Content-Length matches the bytes received", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response("short", {
      status: 206,
      headers: { "Content-Range": `bytes 0-${chunkSize - 1}/${file.size}`, "Content-Length": "5" },
    }));
    vi.stubGlobal("fetch", fetchMock);
    const write = vi.fn();
    const remove = mockBrowserStream(write);

    await download(null, [file]);

    expect(downloadManager.queue[0]).toMatchObject({
      status: "error", errorDetails: "Connection closed before the full chunk was received", abortController: null,
    });
    expect(write).toHaveBeenCalledOnce();
    expect(remove).toHaveBeenCalledOnce();
  });

  it("discards a partial download when cancelled during a range", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(new Response(
      new Uint8Array([1]),
      { status: 206, headers: { "Content-Range": `bytes 0-${chunkSize - 1}/${file.size}` } },
    ));
    vi.stubGlobal("fetch", fetchMock);
    const remove = mockBrowserStream(() => downloadManager.cancel(downloadManager.queue[0].id));

    await download(null, [file]);

    expect(fetchMock.mock.calls[0][1].signal.aborted).toBe(true);
    expect(downloadManager.queue).toHaveLength(0);
    expect(remove).toHaveBeenCalledOnce();
  });

  it("uses native downloads when the browser rejects a transferable stream", async () => {
    prepareBrowserDownload.mockResolvedValueOnce({});
    beginBrowserDownload.mockRejectedValueOnce(new DOMException("Unsupported", "DataCloneError"));
    vi.stubGlobal("fetch", vi.fn().mockResolvedValueOnce(preflightOK).mockResolvedValueOnce(transferComplete(file.size, file.size)));
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    await download(null, [file]);

    expect(downloadManager.queue).toHaveLength(1);
    expect(downloadManager.queue[0]).toMatchObject({ status: "completed", loaded: file.size, progress: 100 });
    expect(click).toHaveBeenCalledOnce();
  });

  it("prepares the share cookie if the browser cannot stream a protected share download", async () => {
    prepareBrowserDownload.mockResolvedValueOnce(null);
    localStorage.setItem("sharepass:requested-share", "passé");
    const fetchMock = vi.fn().mockResolvedValueOnce(preflightOK).mockResolvedValueOnce(transferComplete(file.size, file.size));
    vi.stubGlobal("fetch", fetchMock);
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      expect(new URL(this.href).searchParams.get("hash")).toBe("requested-share");
    });

    await download(null, [file], "requested-share");

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "HEAD", headers: { "X-SHARE-PASSWORD": "pass%C3%A9", "X-Track-Download": "1" } });
    expect(click).toHaveBeenCalledOnce();
    expect(downloadManager.queue[0]).toMatchObject({ status: "completed", loaded: file.size, progress: 100 });
  });
});

it("shows native single-file progress using the server's byte count and size", async () => {
  const file = { type: "file", path: "/hello.txt", source: "files", size: 16 };
  const { promise, resolve } = Promise.withResolvers();
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(preflightOK)
    .mockResolvedValueOnce({ ok: true, json: async () => ({ status: "downloading", bytes: 6, total: 12 }) })
    .mockReturnValueOnce(promise);
  vi.stubGlobal("fetch", fetchMock);
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

  const transfer = download(null, [file]);
  await vi.waitFor(() => expect(downloadManager.queue[0]).toMatchObject({ status: "downloading", loaded: 6, size: 12, progress: 50 }));
  expect(click).toHaveBeenCalledOnce();
  expect(prepareBrowserDownload).not.toHaveBeenCalled();

  resolve(transferComplete(12, 12));
  await transfer;
  expect(downloadManager.queue[0]).toMatchObject({ status: "completed", loaded: 12, size: 12, progress: 100 });
});

describe("archive download", () => {
  it("shows progress for a protected share while the browser saves the archive", async () => {
    localStorage.setItem("sharepass:requested-share", "passé");
    localStorage.setItem("sharepass:different-share", "other-password");

    const fetchMock = vi.fn()
      .mockResolvedValueOnce(preflightOK)
      .mockResolvedValueOnce(transferComplete(65536));
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      const params = new URL(this.href).searchParams;
      expect(params.get("hash")).toBe("requested-share");
      expect(params.get("downloadId")).toBe("tracked-id");
    });

    await download("zip", [folder], "requested-share");

    expect(mutations.showPrompt).toHaveBeenCalledWith({ name: "download" });
    expect(downloadManager.queue[0]).toMatchObject({ name: "backup.zip", status: "completed", loaded: 65536 });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][1]).toMatchObject({
      method: "HEAD",
      headers: { "X-SHARE-PASSWORD": "pass%C3%A9", "X-Track-Download": "1" },
    });
    expect(fetchMock.mock.calls[1][0]).toContain("/public/api/resources/progress/download/tracked-id");
    expect(nativeDownload).toHaveBeenCalledTimes(1);
  });

  it("starts the archive in the browser before progress completes and fetches only metadata", async () => {
    const { promise, resolve } = Promise.withResolvers();
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({
        ...preflightOK,
        headers: new Headers({
          "X-Download-ID": "tracked-id",
          "Content-Disposition": "attachment; filename*=UTF-8''saved%20archive.zip",
        }),
      })
      .mockReturnValueOnce(promise);
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      const url = new URL(this.href);
      expect(url.pathname).toBe("/api/resources/download");
      expect(url.searchParams.get("downloadId")).toBe("tracked-id");
    });

    const transfer = download("zip", [folder]);
    await vi.waitFor(() => expect(nativeDownload).toHaveBeenCalledOnce());
    expect(downloadManager.queue[0]).toMatchObject({ name: "saved archive.zip", status: "pending" });

    resolve(transferComplete(52));
    await transfer;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][1].method).toBe("HEAD");
    expect(fetchMock.mock.calls[1][0]).toBe("/api/resources/progress/download/tracked-id");
    expect(downloadManager.queue[0]).toMatchObject({ status: "completed", loaded: 52 });
  });

  it("reports unavailable progress without cancelling the browser's transfer", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(preflightOK)
      .mockResolvedValueOnce({ ok: false, status: 503 });
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    await download("zip", [folder]);

    expect(downloadManager.queue[0]).toMatchObject({
      status: "error",
      errorDetails: expect.stringContaining("progress is unavailable"),
    });
    expect(nativeDownload).toHaveBeenCalledOnce();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("shows an archive size rejection without starting the browser download", async () => {
    localStorage.setItem("sharepass:different-share", "other-password");
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 413 });
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    await download("zip", [folder]);

    expect(downloadManager.queue[0]).toMatchObject({
      status: "error",
      errorDetails: expect.stringContaining("archive download limit"),
    });
    expect(fetchMock.mock.calls[0][1].headers).toEqual({ "X-Track-Download": "1" });
    expect(nativeDownload).not.toHaveBeenCalled();
  });

  it("cancels the server transfer when the download is cancelled in the panel", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(preflightOK)
      .mockImplementationOnce(async (_url, options) => {
        downloadManager.cancel(downloadManager.queue[0].id);
        expect(options.signal.aborted).toBe(true);
        throw new DOMException("Download cancelled", "AbortError");
      })
      .mockResolvedValueOnce({ ok: true, status: 204 });
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    await download("tar.gz", [folder]);

    expect(fetchMock.mock.calls[2][0]).toBe("/api/resources/progress/download/tracked-id");
    expect(fetchMock.mock.calls[2][1]).toMatchObject({ method: "DELETE", keepalive: true });
    expect(downloadManager.queue).toHaveLength(0);
  });
});
