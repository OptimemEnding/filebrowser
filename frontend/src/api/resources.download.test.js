import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/utils/constants", () => ({
  globalVars: { baseURL: "/", externalUrl: "" },
}));

vi.mock("@/store", () => ({
  getters: { isShare: () => false },
  state: {
    sessionId: "test-session",
    shareInfo: { hash: "different-share" },
    user: { fileLoading: { downloadChunkSizeMb: 1 } },
  },
}));

import { download } from "@/api/resources.js";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  localStorage.clear();
});

describe("archive download", () => {
  it("preflights the requested protected share and starts a native download even when chunking is configured", async () => {
    localStorage.setItem("sharepass:requested-share", "passé");
    localStorage.setItem("sharepass:different-share", "other-password");

    const fetchMock = vi.fn().mockResolvedValue({ ok: true, status: 200 });
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function () {
      expect(new URL(this.href).searchParams.get("hash")).toBe("requested-share");
      expect(new URL(this.href).searchParams.has("archiveToken")).toBe(false);
    });

    await download("zip", [{ type: "directory", path: "/" }], "requested-share");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][1]).toMatchObject({
      method: "HEAD",
      headers: { "X-SHARE-PASSWORD": "pass%C3%A9" },
    });
    expect(nativeDownload).toHaveBeenCalledTimes(1);
  });

  it("reports an archive size rejection without starting a download or sending share credentials", async () => {
    localStorage.setItem("sharepass:different-share", "other-password");
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 413 });
    vi.stubGlobal("fetch", fetchMock);
    const nativeDownload = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});

    await expect(download("zip", [{ type: "directory", path: "/backup", source: "files" }]))
      .rejects.toThrow("archive download limit");

    expect(fetchMock.mock.calls[0][1].headers).toEqual({});
    expect(nativeDownload).not.toHaveBeenCalled();
  });
});
