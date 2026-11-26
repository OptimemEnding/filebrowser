import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/utils/constants", () => ({
  globalVars: { baseURL: "/", externalUrl: "" },
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

afterEach(() => {
  downloadManager.reset();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.clearAllMocks();
  localStorage.clear();
});

const preflightOK = { ok: true, status: 200, headers: new Headers({ "X-Archive-Download-ID": "tracked-id" }) };
const folder = { type: "directory", path: "/backup", source: "files" };

describe("archive download", () => {
  it("shows progress for a protected share while the browser saves the archive", async () => {
    localStorage.setItem("sharepass:requested-share", "passé");
    localStorage.setItem("sharepass:different-share", "other-password");

    const fetchMock = vi.fn()
      .mockResolvedValueOnce(preflightOK)
      .mockResolvedValueOnce({ ok: true, json: async () => ({ status: "completed", bytes: 65536 }) });
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      const params = new URL(this.href).searchParams;
      expect(params.get("hash")).toBe("requested-share");
      expect(params.get("archiveDownload")).toBe("tracked-id");
    });

    await download("zip", [folder], "requested-share");

    expect(mutations.showPrompt).toHaveBeenCalledWith({ name: "download" });
    expect(downloadManager.queue[0]).toMatchObject({ name: "backup.zip", status: "completed", loaded: 65536 });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[0][1]).toMatchObject({
      method: "HEAD",
      headers: { "X-SHARE-PASSWORD": "pass%C3%A9", "X-Track-Archive": "1" },
    });
    expect(fetchMock.mock.calls[1][0]).toContain("/public/api/resources/archive-download/tracked-id");
    expect(nativeDownload).toHaveBeenCalledTimes(1);
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
    expect(fetchMock.mock.calls[0][1].headers).toEqual({ "X-Track-Archive": "1" });
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

    expect(fetchMock.mock.calls[2][0]).toBe("/api/resources/archive-download/tracked-id");
    expect(fetchMock.mock.calls[2][1]).toMatchObject({ method: "DELETE", keepalive: true });
    expect(downloadManager.queue).toHaveLength(0);
  });
});
