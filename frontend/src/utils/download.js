import { resourcesApi } from "@/api/index.js";
import { notify } from "@/notify/index.ts";
import { getters, mutations, state } from "@/store/index.ts";
import { isDirectoryItem } from "@/utils/directoryItem.js";

function isSingleFileItem(items) {
  return items.length === 1 && !isDirectoryItem(items[0]);
}

export default function downloadFiles(items) {
  if (items.length === 0) {
    notify.showError("No files selected");
    return;
  }
  if (typeof items[0] === "number") {
    // map the index to state.req.items
    items = items.map(i => state.req.items.at(i));
  }
  if (getters.isShare()) {
    // Perform download without opening a new window
    if (isSingleFileItem(items)) {
      void startDownload(null, items, state.shareInfo.hash);
    } else {
      // Multiple files download with user confirmation
      mutations.showPrompt({
        name: "download",
        confirm: (format, directArchive = false) => {
          mutations.closeTopPrompt();
          void startDownload(format, items, state.shareInfo.hash, { directArchive });
        },
      });
    }
    return;
  }

  if (isSingleFileItem(items)) {
    void startDownload(null, items);
  } else {
    // Multiple files download with user confirmation
    mutations.showPrompt({
      name: "download",
      confirm: (format, directArchive = false) => {
        mutations.closeTopPrompt();
        void startDownload(format, items, "", { directArchive });
      },
    });
  }
}

async function startDownload(config, files, hash = "", options = {}) {
  try {
    notify.showSuccessToast("Downloading...");
    await resourcesApi.download(config, files, hash, { directArchive: options.directArchive });
  } catch (e) {
    if (e?.name === "AbortError" || e?.message?.includes("aborted") || e?.message?.includes("cancelled")) {
      return;
    }
    notify.showError(`Error downloading: ${e.message || e}`);
  }
}

/** Show the download format prompt and start the in-app download when confirmed. */
export function showShareDownloadPrompt(items) {
  if (items.length === 0) {
    notify.showError("No files selected");
    return;
  }
  if (typeof items[0] === "number") {
    items = items.map((i) => state.req.items.at(i));
  }

  if (isSingleFileItem(items)) {
    void startDownload(null, items, state.shareInfo?.hash || "");
    return;
  }

  mutations.showPrompt({
    name: "download",
    confirm: (format, directArchive = false) => {
      mutations.closeTopPrompt();
      void startDownload(format, items, state.shareInfo?.hash || "", { directArchive });
    },
  });
}
