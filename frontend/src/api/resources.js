import i18n from "@/i18n/index.ts"
import { notify } from "@/notify/index.ts"
import { getters, mutations, state } from "@/store/index.ts"
import { globalVars } from "@/utils/constants.ts"
import { downloadManager } from "@/utils/downloadManager.ts"
import {
  notifyDownloadComplete,
  notifyDownloadError,
} from "@/utils/appNotifications.js"
import { renew } from "@/utils/auth.js"
import { getApiPath, getPublicApiPath } from '@/utils/url.js'
import { adjustedData, fetchURL } from "./utils.ts"
import { rememberViewToken } from "./viewToken.js"
import { isMediaFile } from "@/utils/mediaFile.js"
import { isDirectoryItem } from "@/utils/directoryItem.js"
import { getObjectProperty } from "@/utils/object.js"
import { beginBrowserDownload, prepareBrowserDownload } from "@/utils/streamDownload.js"
import { getStreamURL } from "./media.ts"
import {
  invalidateDirMetadataItem,
  markDirMetadataItemChanged,
  removeDirMetadataItem
} from '@/utils/cache/metadataCache.ts'

export { fetchPreviewBlob, fetchPreviewImage } from "@/utils/previewRequests.ts"

const VIEW_TOKEN_TTL_SECONDS = 15 * 60;
const MOCK_DATA_SOURCE = 'mockData';
const MOCK_DATA_DEFAULT_COUNT = 1000;
const MOCK_DATA_MAX_COUNT = 100_000;

function isMockDataSource(source) {
  return !!(globalVars.devMode || globalVars.playwrightTest) && source === MOCK_DATA_SOURCE;
}

function parseMockDataCount(value, defaultCount) {
  if (value === undefined || value === null || value === '') {
    return defaultCount;
  }
  const n = Math.trunc(Number(value));
  if (!Number.isFinite(n) || n < 0) {
    return defaultCount;
  }
  return Math.min(n, MOCK_DATA_MAX_COUNT);
}

function getMockDataCounts() {
  const query = state.route?.query ?? {};
  const seedRaw = query.seed;
  const seed =
    seedRaw === undefined || seedRaw === null || seedRaw === ''
      ? undefined
      : String(seedRaw);
  return {
    numDirs: parseMockDataCount(query.numDirs, MOCK_DATA_DEFAULT_COUNT),
    numFiles: parseMockDataCount(query.numFiles, MOCK_DATA_DEFAULT_COUNT),
    seed,
  };
}

function normalizeMockListing(raw) {
  const data = { ...raw };
  data.type = 'directory';
  data.source = MOCK_DATA_SOURCE;
  data.path = '/';
  data.name = MOCK_DATA_SOURCE;
  // The backend already types folder rows as 'directory'; this stays as a
  // defensive default so a partial/older mock payload still renders as folders.
  if (Array.isArray(data.folders)) {
    data.folders = data.folders.map((folder) => ({
      ...folder,
      type: folder.type || 'directory',
    }));
  }
  return data;
}

async function fetchMockDataListing(numDirs, numFiles, seed) {
  const apiPath = getApiPath('mock-data', {
    numDirs: String(numDirs),
    numFiles: String(numFiles),
    seed,
  });
  const res = await fetchURL(apiPath);
  return res.json();
}

function chunkUploadApiPath(apiPath, headers) {
  if (headers["X-File-Chunk-Offset"] === undefined) {
    return apiPath;
  }
  const sep = apiPath.includes("?") ? "&" : "?";
  return `${apiPath}${sep}_chunkSeq=${headers["X-File-Chunk-Offset"]}`;
}

function cacheViewTokenFromListing(data) {
  const scope = getters.isShare()
    ? state.shareInfo?.hash || data?.source
    : data?.source;
  if (!scope) {
    return;
  }
  const expiresAt = Math.floor(Date.now() / 1000) + VIEW_TOKEN_TTL_SECONDS;
  if (data.viewToken) {
    rememberViewToken(scope, data.viewToken, expiresAt);
    return;
  }
  const token =
    data.items?.find((item) => item.viewToken)?.viewToken ??
    data.files?.find((item) => item.viewToken)?.viewToken;
  if (token) {
    rememberViewToken(scope, token, expiresAt);
  }
}

// Notify if errors occur
export async function fetchFiles(source, path, content = false, metadata = false, skipExtendedAttrs = false, signal = undefined) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    if (isMockDataSource(source)) {
      if (content || metadata) {
        throw new Error('mockData does not support file content or metadata');
      }
      const normalizedPath = path === '' ? '/' : path;
      if (normalizedPath !== '/') {
        const err = new Error('mockData only supports the root listing');
        err.status = 404;
        throw err;
      }
      const { numDirs, numFiles, seed } = getMockDataCounts();
      const raw = await fetchMockDataListing(numDirs, numFiles, seed);
      const adjusted = adjustedData(normalizeMockListing(raw));
      cacheViewTokenFromListing(adjusted);
      return adjusted;
    }
    const apiPath = getApiPath('resources', {
      path: path,
      source: source,
      ...(content && { content: 'true' }),
      ...(metadata && { metadata: 'true' }),
      ...(skipExtendedAttrs && { skipExtendedAttrs: 'true' })
    })
    const res = await fetchURL(apiPath, signal ? { signal } : undefined)
    const data = await res.json()
    const adjusted = adjustedData(data)
    cacheViewTokenFromListing(adjusted)
    return adjusted
  } catch (err) {
    if (err?.name !== 'AbortError') {
      notify.showError(err.message || 'Error fetching data')
    }
    throw err
  }
}

/**
 * Lists a directory as a Map of name -> { size, type } without showing error toasts.
 * Returns null when the directory can't be listed (e.g. it doesn't exist yet), so
 * callers can treat every item in it as missing.
 */
export async function listDirectoryEntries(source, path) {
  try {
    const res = await fetchURL(getApiPath('resources', { path, source }))
    const data = adjustedData(await res.json())
    if (data.type !== 'directory') {
      return null
    }
    return new Map(data.items.map((item) => [item.name, { size: item.size, type: item.type }]))
  } catch {
    return null
  }
}

export async function getItems(source, path, only = "") {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const apiPath = getApiPath('resources/items', {
      path: path,
      source: source,
      ...(only && { only: only }),
    })
    const res = await fetchURL(apiPath)
    const data = await res.json()
    return data
  } catch (err) {
    notify.showError(err.message || 'Error fetching items')
    throw err
  }
}

/**
 * Registers a graceful pause for the active chunked upload at this path.
 * Call immediately before aborting the chunk XHR so the server keeps partial data.
 */
export async function signalUploadPause(source, path, shareHash) {
  if (shareHash) {
    const apiPath = getPublicApiPath('resources/pause', {
      hash: shareHash,
      path: path,
    })
    await fetchURL(apiPath, { method: 'POST', headers: sharePublicAuthHeaders(shareHash) })
    return
  }
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  const apiPath = getApiPath('resources/pause', { path, source })
  await fetchURL(apiPath, { method: 'POST' })
}

async function resourceAction(source, path, method, content) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const apiPath = getApiPath('resources', { path, source })
    const opts = { method }
    if (content) {
      opts.body = content
    }

    const response = await fetchURL(apiPath, opts)
    if (!response.ok) {
      const error = new Error(response.statusText);
      // attempt to marshal json response
      let data = null;
      try {
        data = await response.json()
      } catch (_e) {
        // ignore
      }
      if (data) {
        error.message = data.message;
      }
      (/** @type {any} */ (error)).status = response.status;
      throw error;
    }
    return response
  } catch (err) {
    notify.showError(err.message || 'Error performing resource action')
    throw err
  }
}

export async function bulkDelete(items) {
  if (!items || !Array.isArray(items) || items.length === 0) {
    throw new Error('items array is required and must not be empty')
  }
  try {
    const apiPath = getApiPath('resources/bulk')
    const response = await fetch(apiPath, {
      method: 'DELETE',
      headers: {
        'Content-Type': 'application/json',
        'sessionId': state.sessionId,
      },
      credentials: 'same-origin',
      body: JSON.stringify(items),
    })
    const data = await response.json()
    // 200 = all succeeded, 207 = partial success (some succeeded, some failed)
    if (response.status === 200 || response.status === 207) {
      for (const item of items) {
        if (response.status === 200) {
          removeDirMetadataItem({ source: item.source }, item.path)
        } else {
          invalidateDirMetadataItem({ source: item.source }, item.path)
        }
      }
      return data
    }
    const error = new Error(data.message || response.statusText)
    error.status = response.status
    if (data.failed) {
      error.failed = data.failed
    }
    if (data.succeeded) {
      error.succeeded = data.succeeded
    }
    throw error
  } catch (err) {
    if (!err.status || (err.status !== 500 && err.status !== 200 && err.status !== 207)) {
      notify.showError(err.message || 'Error performing bulk delete')
    }
    throw err
  }
}

export async function put(source, path, content = '') {
  if (!source) {
    throw new Error('no source provided')
  }
  // resourceAction already handles error notification, just propagate
  const result = await resourceAction(source, path, 'PUT', content)
  markDirMetadataItemChanged({ source }, path)
  return result
}

export async function download(format, files, shareHash = "") {
  const isMultiItemArchive =
    files.length > 1 || (files.length === 1 && isDirectoryItem(files[0]))

  if (format !== 'zip') {
    format = 'tar.gz'
  }

  let source = null
  let filePaths = []

  if (shareHash) {
    filePaths = files.map((file) => file.path)
  } else {
    for (const file of files) {
      if (!file.source) {
        throw new Error('File source is required for downloads')
      }
      if (source === null) {
        source = file.source
      } else if (source !== file.source) {
        throw new Error('All files must be from the same source for downloads')
      }
      filePaths.push(file.path)
    }
  }

  const params = {
    file: filePaths,
    algo: format,
    ...(shareHash && { hash: shareHash }),
    ...(!shareHash && source && { source: source }),
    sessionId: state.sessionId,
  }

  const apiPath = getApiPath("resources/download", params, false, shareHash !== "")
  const url = window.origin + apiPath

  if (isMultiItemArchive) {
    const stem = files.length === 1
      ? files[0].name || files[0].path?.split('/').filter(Boolean).at(-1)
      : undefined
    return downloadNative(url, { name: `${stem || 'download'}.${format}` }, shareHash, { format, files })
  }

  const chunkSize = Math.floor((state.user?.fileLoading?.downloadChunkSizeMb || 0) * 1024 * 1024)
  const fileSize = Number(files[0]?.size)
  if (chunkSize > 0 && Number.isSafeInteger(fileSize) && fileSize >= chunkSize) {
    const delivery = await prepareBrowserDownload()
    if (delivery && await downloadChunked(delivery, url, files[0], fileSize, chunkSize, shareHash)) return
  }
  return downloadNative(url, files[0], shareHash)
}

function startNativeDownload(url) {
  const link = document.createElement('a')
  link.href = url
  link.style.display = 'none'
  document.body.appendChild(link)
  link.click()
  setTimeout(() => link.remove(), 100)
}

async function downloadNative(url, file, shareHash, archiveRequest) {
  const id = downloadManager.add(file, shareHash)
  const item = downloadManager.findById(id)
  const controller = new AbortController()
  const { signal } = controller
  item.abortController = controller
  item.archiveRequest = archiveRequest
  showDownloadPanel()

  try {
    const response = await fetch(url, {
      ...shareDownloadFetchInit(shareHash, 'HEAD', { 'X-Track-Download': '1' }),
      cache: 'no-store',
      signal,
    })
    if (response.status === 413) {
      throw new Error(i18n.global.t('fileLoading.archiveDownloadTooLarge'))
    }
    if (!response.ok) {
      throw new Error(response.statusText || `HTTP ${response.status}`)
    }

    const transferId = response.headers.get('X-Download-ID')
    if (!transferId) {
      throw new Error(i18n.global.t('fileLoading.downloadProgressUnavailable'))
    }
    const attachmentName = response.headers.get('Content-Disposition')
      ?.match(/filename\*=UTF-8''([^;]+)/i)?.[1]
    if (attachmentName) {
      try {
        item.name = decodeURIComponent(attachmentName)
      } catch {
        // Keep the name built from the selection.
      }
    }
    const size = response.headers.get('Content-Length')
    if (!archiveRequest && size !== null && Number.isSafeInteger(Number(size))) {
      item.size = Number(size)
    }
    const downloadURL = new URL(url)
    downloadURL.searchParams.set('downloadId', transferId)
    const progressURL = getApiPath(
      `resources/progress/download/${encodeURIComponent(transferId)}`,
      { hash: shareHash }, false, Boolean(shareHash),
    )
    const cancelDownload = () => {
      void fetch(progressURL, {
        ...shareDownloadFetchInit(shareHash, 'DELETE'),
        keepalive: true,
      }).catch(() => {})
    }
    if (signal.aborted) {
      cancelDownload()
      return
    }

    signal.addEventListener('abort', cancelDownload, { once: true })
    try {
      startNativeDownload(downloadURL.href)
      const status = await waitForDownloadProgress(progressURL, shareHash, item, signal)
      if (!signal.aborted) downloadManager.setStatus(id, status)
    } finally {
      signal.removeEventListener('abort', cancelDownload)
    }
  } catch (error) {
    if (!signal.aborted) downloadManager.setError(id, error.message)
  } finally {
    item.abortController = null
  }
}

async function waitForDownloadProgress(url, shareHash, item, signal) {
  const startDeadline = Date.now() + 30_000
  while (!signal.aborted) {
    const response = await fetch(url, {
      ...shareDownloadFetchInit(shareHash, 'GET'),
      cache: 'no-store',
      signal,
    })
    if (!response.ok) {
      throw new Error(i18n.global.t('fileLoading.downloadProgressUnavailable'))
    }

    const progress = await response.json()
    if (progress.status === 'error') {
      throw new Error(i18n.global.t('notifications.downloadFailedTitle'))
    }
    if (progress.status === 'pending' && Date.now() > startDeadline) {
      throw new Error(i18n.global.t('fileLoading.downloadDidNotStart'))
    }
    if (Number.isSafeInteger(progress.total) && progress.total > 0) item.size = progress.total
    downloadManager.updateProgress(item.id, progress.bytes, Math.max(item.size, progress.bytes))
    if (progress.status === 'completed' || progress.status === 'cancelled') {
      return progress.status
    }
    downloadManager.setStatus(item.id, progress.status)
    await new Promise((resolve) => { setTimeout(resolve, 1000) })
  }
  signal.throwIfAborted()
}

function showDownloadPanel() {
  if (!state.prompts.some((prompt) => prompt.name === 'download' && !prompt.confirm)) {
    mutations.showPrompt({ name: 'download' })
  }
}

function shareDownloadFetchInit(shareHash, method, extraHeaders = {}) {
  return {
    method,
    credentials: "same-origin",
    headers: {
      ...(shareHash ? sharePublicAuthHeaders(shareHash) : {}),
      ...extraHeaders,
    },
  };
}

/** Best-effort text from a failed download response (consumes body). */
async function readDownloadErrorBody(response) {
  try {
    const text = await response.text()
    if (text && text.length > 0) {
      return text.length > 300 ? `${text.slice(0, 300)}…` : text
    }
  } catch {
    // ignore
  }
  return ''
}

function parseDownloadContentRange(header) {
  const match = /^bytes (\d+)-(\d+)\/(\d+)$/i.exec(header ?? '')
  if (!match) return null

  const [start, end, total] = match.slice(1).map(Number)
  if (![start, end, total].every(Number.isSafeInteger) || start > end || end >= total) {
    return null
  }
  return { start, end, total }
}

async function* readDownloadRanges(url, chunkSize, downloadItem, signal, shareHash) {
  let offset = 0
  let totalSize = downloadItem.size
  let validator = null

  while (offset < totalSize) {
    signal.throwIfAborted()

    const end = Math.min(offset + chunkSize - 1, totalSize - 1)
    const rangeHeader = `bytes=${offset}-${end}`

    const response = await fetch(url, {
      ...shareDownloadFetchInit(shareHash, 'GET', {
        Range: rangeHeader,
        ...(validator && { 'If-Range': validator }),
      }),
      cache: 'no-store',
      signal,
    })

    if (!response.ok) {
      const body = (await readDownloadErrorBody(response)).trim()
      const fromHttp =
        body ||
        [response.statusText, `HTTP ${response.status}`].filter(Boolean).join(' ').trim()
      throw new Error(fromHttp || 'Request failed')
    }

    let expectedChunkSize = null
    if (response.status === 206) {
      const range = parseDownloadContentRange(response.headers.get('Content-Range'))
      if (
        !range || range.start !== offset || range.end > end ||
        (offset > 0 && range.total !== totalSize)
      ) {
        throw new Error('Invalid Content-Range in download response')
      }
      totalSize = range.total
      expectedChunkSize = range.end - range.start + 1
      const etag = response.headers.get('ETag')
      const responseValidator = etag && !etag.startsWith('W/') ? etag : response.headers.get('Last-Modified')
      if (validator && responseValidator && responseValidator !== validator) {
        throw new Error('The source file changed during download')
      }
      if (offset === 0) validator = responseValidator
    } else if (response.status === 200 && offset === 0) {
      const contentLength = response.headers.get('Content-Length')
      const encoding = response.headers.get('Content-Encoding')
      const length = Number(contentLength)
      if (
        contentLength !== null && (!encoding || encoding === 'identity') &&
        Number.isSafeInteger(length) && length >= 0
      ) {
        totalSize = length
        expectedChunkSize = length
      }
    } else {
      throw new Error(`Unexpected HTTP ${response.status} for download range ${rangeHeader}`)
    }
    downloadItem.size = totalSize

    if (!response.body) {
      throw new Error('No response body')
    }
    let chunkLoaded = 0
    let lastProgressUpdate = 0
    const progressUpdateInterval = Math.max(50000, (expectedChunkSize ?? totalSize) / 50)

    const reader = response.body.getReader()
    try {
      while (true) {
        const { done, value } = await reader.read()
        signal.throwIfAborted()
        if (done) break
        chunkLoaded += value.byteLength
        if (expectedChunkSize !== null && chunkLoaded > expectedChunkSize) {
          throw new Error('Download response exceeded its expected size')
        }
        if (
          chunkLoaded - lastProgressUpdate >= progressUpdateInterval ||
          chunkLoaded === expectedChunkSize
        ) {
          const loaded = offset + chunkLoaded
          downloadManager.updateProgress(downloadItem.id, loaded, Math.max(totalSize, loaded))
          lastProgressUpdate = chunkLoaded
        }
        yield value
      }
    } finally {
      reader.releaseLock()
    }

    if (expectedChunkSize !== null && chunkLoaded !== expectedChunkSize) {
      throw new Error('Connection closed before the full chunk was received')
    }

    offset += chunkLoaded
    if (response.status === 200) {
      totalSize = offset
      downloadItem.size = totalSize
    }
    downloadManager.updateProgress(downloadItem.id, offset, totalSize)
  }
}

async function downloadChunked(delivery, url, file, fileSize, chunkSize, shareHash) {
  const fileName = file.name || file.path?.split('/').filter(Boolean).at(-1) || 'download'
  const downloadId = downloadManager.add({ ...file, size: fileSize }, shareHash)
  const downloadItem = downloadManager.findById(downloadId)
  const abortController = new AbortController()
  downloadItem.abortController = abortController
  downloadManager.setStatus(downloadId, 'downloading')

  const { signal } = abortController
  const ranges = readDownloadRanges(url, chunkSize, downloadItem, signal, shareHash)
  const transfer = Promise.withResolvers()
  const body = new ReadableStream({
    async pull(controller) {
      try {
        const { done, value } = await ranges.next()
        if (done) {
          controller.close()
          transfer.resolve()
        } else {
          controller.enqueue(value)
        }
      } catch (error) {
        controller.error(error)
        transfer.reject(error)
      }
    },
    async cancel(reason) {
      transfer.reject(reason instanceof Error ? reason : new DOMException('Download cancelled', 'AbortError'))
      abortController.abort()
      await ranges.return()
    },
  }, { highWaterMark: 0 })

  let iframe
  try {
    await Promise.all([
      beginBrowserDownload(delivery, fileName, body, signal).then((frame) => {
        iframe = frame
        showDownloadPanel()
      }),
      transfer.promise,
    ])
    signal.throwIfAborted()
    downloadManager.setStatus(downloadId, 'completed')
    notifyDownloadComplete(fileName, downloadItem.size)
  } catch (error) {
    const cancelled = signal.aborted || error?.name === 'AbortError'
    abortController.abort()
    if (!iframe) {
      downloadManager.remove(downloadId)
      return cancelled
    }
    if (cancelled) {
      downloadManager.setStatus(downloadId, 'cancelled')
      return true
    }
    const message =
      error instanceof Error ? error.message || "Unknown error" : String(error)
    downloadManager.setError(downloadId, message)
    notifyDownloadError(downloadItem.name, message)
  } finally {
    iframe?.remove()
    downloadItem.abortController = null
  }
  return true
}

/**
 * Uploads a file, one chunk of it, or creates a folder.
 * @param {string | undefined} source
 * @param {string} path
 * @param {string | Blob} [content]
 * @param {boolean} [overwrite]
 * @param {((percent: number) => void) | undefined} [onupload]
 * @param {Record<string, string | number>} [headers]
 * @param {boolean} [isDir]
 * @param {(() => void) | undefined} [onsent]
 * @returns {Promise<any> & { xhr?: XMLHttpRequest | null, abort?: () => void }}
 */
export function post(
  source,
  path,
  content = "",
  overwrite = false,
  onupload,
  headers = {},
  isDir = false,
  onsent
) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const apiPath = getApiPath("resources", {
      path: path,
      source: source,
      override: overwrite,
      ...(isDir && { isDir: 'true' })
    });

    const opState = { xhr: null, aborted: false };

    const rejectIfAborted = (reject) => {
      if (!opState.aborted) {
        return false;
      }
      reject(new Error("Upload aborted"));
      return true;
    };

    const startRequest = (isRetry) =>
      new Promise((resolve, reject) => {
        if (rejectIfAborted(reject)) {
          return;
        }
        const request = new XMLHttpRequest();
        opState.xhr = request;
        const resolvedPath = chunkUploadApiPath(apiPath, headers);
        request.open("POST", resolvedPath, true);

        for (const [header, value] of Object.entries(headers)) {
          request.setRequestHeader(header, value);
        }
        if (headers["X-File-Chunk-Offset"] !== undefined) {
          request.setRequestHeader("Connection", "close");
        }

        if (typeof onupload === "function") {
          request.upload.onprogress = (event) => {
            if (event.lengthComputable) {
              const percentComplete = Math.round(
                (event.loaded / event.total) * 100
              );
              onupload(percentComplete);
            }
          };
        }
        if (typeof onsent === "function") {
          // The whole body has been sent; from here the server answers when it is done saving it.
          request.upload.onload = () => onsent();
        }

        request.onload = () => {
          if (request.status >= 200 && request.status < 300) {
            resolve(request.responseText);
            return;
          }
          if (request.status === 409) {
            const error = new Error("conflict");
            error.response = { status: request.status, responseText: request.responseText };
            reject(error);
            return;
          }
          // Session expired mid-upload: renew once and retry the same body.
          if (request.status === 401 && !isRetry && !getters.isShare()) {
            void (async () => {
              try {
                await renew();
              } catch {
                if (rejectIfAborted(reject)) {
                  return;
                }
                let errorMessage = "Upload failed";
                try {
                  const errorData = JSON.parse(request.responseText);
                  errorMessage = errorData.message || errorMessage;
                } catch (_e) {
                  errorMessage = request.responseText || errorMessage;
                }
                const error = new Error(errorMessage);
                error.status = request.status;
                error.response = { status: request.status, responseText: request.responseText };
                if (request.status !== 507) {
                  notify.showError(errorMessage);
                }
                reject(error);
                return;
              }
              if (rejectIfAborted(reject)) {
                return;
              }
              try {
                resolve(await startRequest(true));
              } catch (err) {
                reject(err);
              }
            })();
            return;
          }
          let errorMessage = "Upload failed";
          try {
            const errorData = JSON.parse(request.responseText);
            errorMessage = errorData.message || errorMessage;
          } catch (_e) {
            errorMessage = request.responseText || errorMessage;
          }
          const error = new Error(errorMessage);
          error.status = request.status;
          error.response = { status: request.status, responseText: request.responseText };
          if (request.status !== 507) {
            notify.showError(errorMessage);
          }
          reject(error);
        };

        request.onerror = () => reject(new Error("Network error"));
        request.onabort = () => reject(new Error("Upload aborted"));

        if (
          content instanceof Blob &&
          !["http:", "https:"].includes(window.location.protocol)
        ) {
          new Response(content).arrayBuffer()
            .then((buffer) => {
              if (rejectIfAborted(reject)) {
                return;
              }
              request.send(buffer);
            })
            .catch((err) => reject(err));
        } else {
          request.send(content);
        }
      });

    const uploadPromise = startRequest(false).then((result) => {
      markDirMetadataItemChanged({ source }, path);
      return result;
    });
    // Preserve .xhr for pause/abort; re-attach after .then() so callers still see it.
    void Object.defineProperty(uploadPromise, "xhr", {
      get: () => opState.xhr,
      enumerable: true,
    });
    // Operation-level abort: stops a pending 401 renew+retry as well as the active XHR.
    uploadPromise.abort = () => {
      opState.aborted = true;
      if (opState.xhr && opState.xhr.readyState !== XMLHttpRequest.DONE) {
        opState.xhr.abort();
      }
    };
    return uploadPromise;
  } catch (err) {
    notify.showError(err.message || "Error posting resource");
    // We are returning a promise, so we should return a rejected promise on error.
    return Promise.reject(err);
  }
}

export async function moveCopy(
  items,
  action = 'copy',
  overwrite = false,
  rename = false
) {
  if (!items || !Array.isArray(items) || items.length === 0) {
    throw new Error('items array is required and must not be empty')
  }

  try {
    // Build request body with proper format
    const requestBody = {
      items: items.map(item => ({
        fromSource: item.fromSource,
        fromPath: item.from,
        toSource: item.toSource,
        toPath: item.to
      })),
      action: action,
      overwrite: overwrite,
      rename: rename
    }

    const apiPath = getApiPath('resources')
    // We use fetch directly here instead of fetchURL because fetchURL throws on non-2xx status,
    // consuming the response body as text. We need to parse the JSON response for 500/207 errors.
    // We need to manually add headers that fetchURL adds.
    const headers = {
      'Content-Type': 'application/json',
      'sessionId': state.sessionId
    }

    const response = await fetch(apiPath, {
      method: 'PATCH',
      headers: headers,
      body: JSON.stringify(requestBody),
      credentials: 'same-origin'
    })

    const data = await response.json()

    // 200 = all succeeded, 207 = partial success (some succeeded, some failed)
    if (response.status === 200 || response.status === 207) {
      for (const item of items) {
        if (response.status !== 200) {
          invalidateDirMetadataItem({ source: item.fromSource }, item.from)
          invalidateDirMetadataItem({ source: item.toSource }, item.to)
          continue
        }
        if (action === 'move') {
          removeDirMetadataItem({ source: item.fromSource }, item.from)
        }
        if (rename) {
          invalidateDirMetadataItem({ source: item.toSource }, item.to)
        } else {
          markDirMetadataItemChanged({ source: item.toSource }, item.isDir ? `${item.to}/` : item.to)
        }
      }
      return data
    }

    // For other error status codes (like 500), preserve the response data
    const failedMsg = Array.isArray(data.failed)
      ? data.failed.map((item) => item?.message).find(Boolean)
      : undefined;
    const error = new Error(failedMsg || data.message || response.statusText || "Error moving/copying resources");
    error.status = response.status;
    if (data.code) {
      error.code = data.code;
    }
    // Attach the response data so the caller can access failed items
    if (data.failed) {
      error.failed = data.failed
    }
    if (data.succeeded) {
      error.succeeded = data.succeeded
    }
    throw error
  } catch (err) {
    // For 500 errors with response data, don't show notification here
    // Let the caller handle it with the attached error data
    // Only show notification for unexpected errors (no status or not 500)
    if (!err.status || (err.status !== 500 && err.status !== 200 && err.status !== 207)) {
      notify.showError(err.message || 'Error moving/copying resources')
    }
    throw err
  }
}

export async function checksum(source, path, algo) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    if (getters.isShare()) {
      if (!getters.isLoggedIn()) {
        throw new Error('login required to generate checksums')
      }
      const hash = state.shareInfo?.hash
      if (!hash) {
        throw new Error('no share hash provided')
      }
      const params = { path, hash, checksum: algo }
      const res = await fetchURL(getPublicApiPath('resources', params), {
        headers: sharePublicAuthHeaders(hash),
      })
      const data = await res.json()
      return getObjectProperty(data.checksums, algo)
    }
    const params = {
      path: path,
      source: source,
      checksum: algo
    }
    const apiPath = getApiPath('resources', params)
    const res = await fetchURL(apiPath)
    const data = await res.json()
    return getObjectProperty(data.checksums, algo)
  } catch (err) {
    notify.showError(err.message || 'Error fetching checksum')
    throw err
  }
}

/**
 * URL to open a file inline in a new browser tab via the download endpoint.
 * Uses inline disposition and respects download permissions and share limits.
 * @param {string} source
 * @param {string} path
 * @param {{ path?: string, hash?: string } | null} [shareInfo]
 */
export function getOpenFileURL(source, path, shareInfo = null) {
  if (isMediaFile(path)) {
    return null
  }
  if (shareInfo) {
    return getDownloadURLPublic(shareInfo, [path], true)
  }
  return getDownloadURL(source, path, true)
}

// GET /api/resources/view — inline non-media bytes via viewToken (not download-metered).
export function getRawViewURL(source, path, viewToken) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  if (!viewToken) {
    throw new Error('view token required')
  }
  try {
    const params = {
      source: source,
      file: path,
      viewToken: viewToken,
      sessionId: state.sessionId,
    }
    const apiPath = getApiPath('resources/view', params)
    return window.origin + apiPath
  } catch (err) {
    notify.showError(err.message || 'Error getting view URL')
    throw err
  }
}

// GET /public/api/resources/view
export function getRawViewURLPublic(share, files, viewToken) {
  if (!viewToken) {
    throw new Error('view token required')
  }
  const fileArray = Array.isArray(files) ? files : [files]
  const params = {
    file: fileArray,
    hash: share.hash,
    viewToken: viewToken,
    sessionId: state.sessionId,
  }
  const apiPath = getPublicApiPath('resources/view', params)
  return window.origin + apiPath
}

/**
 * URL for inline viewing. Routes audio/video to /media/stream and other files to /resources/view.
 * @param {{ hash: string, path?: string | undefined } | null} [shareInfo] Share context for public routes; null/omitted for authenticated routes.
 */
export function getViewURL(source, path, viewToken, shareInfo = null, allowDownloadFallback = false, mimeOrName = '') {
  const typeHint = mimeOrName || path

  if (viewToken) {
    if (isMediaFile(typeHint)) {
      return getStreamURL(source, path, viewToken)
    }
    if (shareInfo) {
      return getRawViewURLPublic(shareInfo, [path], viewToken)
    }
    return getRawViewURL(source, path, viewToken)
  }
  if (!allowDownloadFallback) {
    return null
  }
  if (shareInfo) {
    return getDownloadURLPublic(shareInfo, [path], true)
  }
  return getDownloadURL(source, path, true)
}

export function getDownloadURL(source, path, inline, useExternal) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const params = {
      source: source,
      file: path,
      ...(inline && { inline: 'true' })
    }
    const apiPath = getApiPath('resources/download', params)
    if (globalVars.externalUrl && useExternal) {
      return globalVars.externalUrl + apiPath
    }
    return window.origin + apiPath
  } catch (err) {
    notify.showError(err.message || 'Error getting download URL')
    throw err
  }
}

/**
 * Build a preview API URL. Use fetchPreviewImage(url, signal) to load it;
 * pass an AbortSignal so the caller can cancel on navigation or unmount.
 */
export function getPreviewURL(source, path, modified) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const params = {
      path: path,
      key: Date.parse(modified), // Use modified date as cache key
      source: source,
      inline: 'true'
    }
    const apiPath = getApiPath('resources/preview', params)
    return window.origin + apiPath
  } catch (err) {
    notify.showError(err.message || 'Error getting preview URL')
    throw err
  }
}

/**
 * Build a text preview API URL. Use fetchTextPreview(url, modified) to load it.
 * Separate from getPreviewURL: the text endpoint returns JSON, not an image.
 */
export function getTextPreviewURL(source, path) {
  if (!source || source === undefined || source === null) {
    throw new Error('no source provided')
  }
  try {
    const params = {
      path: path,
      source: source,
      inline: 'true'
    }
    const apiPath = getApiPath('resources/preview/text', params)
    return window.origin + apiPath
  } catch (err) {
    notify.showError(err.message || 'Error getting text preview URL')
    throw err
  }
}

// POST /api/resources/archive - Create an archive
export async function createArchive(opts) {
  const { fromSource, toSource, paths, destination, format, compression, deleteAfter } = opts;
  if (!fromSource || !Array.isArray(paths) || paths.length === 0 || !destination) {
    throw new Error("fromSource, paths, and destination are required");
  }
  const body = {
    fromSource,
    paths,
    destination,
    ...(toSource && toSource !== fromSource && { toSource }),
    ...(format && { format }),
    ...(compression !== undefined && compression !== null && { compression }),
    ...(deleteAfter && { deleteAfter: true }),
  };
  try {
    const apiPath = getApiPath("resources/archive");
    const response = await fetchURL(apiPath, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    return response.json();
  } catch (err) {
    notify.showError(err.message || "Error creating archive");
    throw err;
  }
}

// Shared extraction client, including ZIP filename previews.
export { unarchive } from "./archive.js";

// ============================================================================
// PUBLIC API ENDPOINTS (hash-based authentication)
// ============================================================================

function getSharePasswordFromStorage(hash) {
  return localStorage.getItem(`sharepass:${hash}`) || "";
}

function sharePublicAuthHeaders(hash) {
  const password = getSharePasswordFromStorage(hash);
  if (!password) {
    return {};
  }
  return { "X-SHARE-PASSWORD": encodeURIComponent(password) };
}

// Fetch public share data
/**
 * @param {string} path
 * @param {string} hash
 * @param {string} password
 * @param {boolean} content
 * @param {boolean} metadata
 * @returns {Promise<any>}
 */
export async function fetchFilesPublic(path, hash, password = "", content = false, metadata = false, skipExtendedAttrs = false, signal = undefined) {
  const params = {
    path: path,
    hash,
    ...(skipExtendedAttrs && { skipExtendedAttrs: 'true' }),
    ...(content && { content: 'true' }),
    ...(metadata && { metadata: 'true' }),
  }
  const apiPath = getPublicApiPath("resources", params);
  const sharePassword = password || getSharePasswordFromStorage(hash);
  const response = await fetch(apiPath, {
    credentials: "same-origin",
    headers: sharePassword ? { "X-SHARE-PASSWORD": encodeURIComponent(sharePassword) } : {},
    ...(signal && { signal }),
  });

  if (!response.ok) {
    const error = new Error(response.statusText);
    let data = null;
    try {
      data = await response.json()
    } catch (_e) {
      // ignore
    }
    if (data) {
      error.message = data.message;
    }
    (/** @type {any} */ (error)).status = response.status;
    throw error;
  }
  const data = await response.json()
  const adjusted = adjustedData(data);
  cacheViewTokenFromListing(adjusted);
  return adjusted
}

export async function getItemsPublic(hash, path, only = "") {
  if (!hash || hash === undefined || hash === null) {
    throw new Error('no hash provided')
  }
  try {
    const apiPath = getPublicApiPath('resources/items', {
      path: path,
      hash: hash,
      ...(only && { only: only }),
    })
    const response = await fetch(apiPath, { headers: sharePublicAuthHeaders(hash) })
    const data = await response.json()
    return data
  } catch (err) {
    notify.showError(err.message || 'Error fetching items')
    throw err
  }
}

// Generate a download URL
/**
 * @param {{ path: string; hash: string; inline?: boolean }} share
 * @param {string[]} files - Array of file paths (will be converted to repeated 'file' parameters)
 * @returns {string}
 */
export function getDownloadURLPublic(share, files, inline=false) {
  const fileArray = Array.isArray(files) ? files : [files]
  const params = {
    file: fileArray,
    hash: share.hash,
    ...(inline && { inline: 'true' })
  }
  const apiPath = getPublicApiPath("resources/download", params)
  return window.origin + apiPath
}

// Generate a preview URL for public shares
/**
 * @param {string} path
 * @param {string} size - The size parameter (small, large, original). Omit for default (small).
 * @returns {string}
 */
export function getPreviewURLPublic(path, size, viewToken = state.req?.viewToken) {
  try {
    const params = {
      path: path,
      hash: state.shareInfo.hash,
      inline: 'true',
      ...(size && size !== 'small' && { size: size }),
      ...(viewToken && { viewToken }),
    }
    const apiPath = getPublicApiPath('resources/preview', params)
    return window.origin + apiPath
  } catch (/** @type {any} */ err) {
    notify.showError(err.message || 'Error getting preview URL')
    throw err
  }
}

// Generate a text preview URL for public shares
/**
 * @param {string} path
 * @param {string} [viewToken]
 * @returns {string}
 */
export function getTextPreviewURLPublic(path, viewToken = state.req?.viewToken) {
  try {
    const params = {
      path: path,
      hash: state.shareInfo.hash,
      inline: 'true',
      ...(viewToken && { viewToken }),
    }
    const apiPath = getPublicApiPath('resources/preview/text', params)
    return window.origin + apiPath
  } catch (/** @type {any} */ err) {
    notify.showError(err.message || 'Error getting text preview URL')
    throw err
  }
}

/**
 * Uploads a file, one chunk of it, or creates a folder.
 * @param {string | undefined} hash
 * @param {string} path
 * @param {string | Blob} [content]
 * @param {boolean} [overwrite]
 * @param {((percent: number) => void) | undefined} [onupload]
 * @param {Record<string, string | number>} [headers]
 * @param {boolean} [isDir]
 * @param {(() => void) | undefined} [onsent]
 * @returns {Promise<any> & { xhr?: XMLHttpRequest | null, abort?: () => void }}
 */
export function postPublic(
  hash,
  path,
  content = "",
  overwrite = false,
  onupload,
  headers = {},
  isDir = false,
  onsent
) {
  if (!hash || hash === undefined || hash === null) {
    throw new Error('no hash provided')
  }
  Object.assign(headers, sharePublicAuthHeaders(hash));
  try {
    const apiPath = getPublicApiPath("resources", {
      path: path,
      hash: hash,
      override: overwrite,
      ...(isDir && { isDir: 'true' }),
    });

    const request = new XMLHttpRequest();
    request.open("POST", chunkUploadApiPath(apiPath, headers), true);

    for (const [header, value] of Object.entries(headers)) {
      request.setRequestHeader(header, value);
    }
    if (headers["X-File-Chunk-Offset"] !== undefined) {
      request.setRequestHeader("Connection", "close");
    }

    if (typeof onupload === "function") {
      request.upload.onprogress = (event) => {
        if (event.lengthComputable) {
          const percentComplete = Math.round(
            (event.loaded / event.total) * 100
          );
          onupload(percentComplete);
        }
      };
    }
    if (typeof onsent === "function") {
      // The whole body has been sent; from here the server answers when it is done saving it.
      request.upload.onload = () => onsent();
    }

    const promise = new Promise((resolve, reject) => {
      request.onload = () => {
        if (request.status >= 200 && request.status < 300) {
          resolve(request.responseText);
        } else if (request.status === 409) {
          const error = new Error("conflict");
          error.response = { status: request.status, responseText: request.responseText };
          reject(error);
        } else {
          let errorMessage = "Upload failed";
          try {
            const errorData = JSON.parse(request.responseText);
            errorMessage = errorData.message || errorMessage;
          } catch (_e) {
            errorMessage = request.responseText || errorMessage;
          }

          const error = new Error(errorMessage);
          error.status = request.status;
          error.response = { status: request.status, responseText: request.responseText };
          if (request.status !== 507) {
            notify.showError(errorMessage);
          }
          reject(error);
        }
      };

      request.onerror = () => {
        const error = new Error("Network error");
        notify.showError("Network error during upload");
        reject(error);
      };

      request.onabort = () => {
        const error = new Error("Upload aborted");
        notify.showError("Upload was aborted");
        reject(error);
      };

      if (
        content instanceof Blob &&
        !["http:", "https:"].includes(window.location.protocol)
      ) {
        new Response(content).arrayBuffer()
          .then(buffer => request.send(buffer))
          .catch(err => reject(err));
      } else {
        request.send(content);
      }
    });

    promise.xhr = request;
    promise.then(() => {
      markDirMetadataItemChanged({ isShare: true, hash }, path)
    }).catch(() => { /* upload failed so do nothing */ });
    return promise;
  } catch (err) {
    notify.showError(err.message || "Error posting resource");
    return Promise.reject(err);
  }
}

async function resourceActionPublic(hash, path, method, content, token = "") {
  try {
    const headers = sharePublicAuthHeaders(hash);
    const apiPath = getPublicApiPath('resources', {
      path,
      hash: hash,
      ...(token && { token }),
    })
    const response = await fetch(apiPath, {
      method,
      body: content,
      headers,
    });
    if (!response.ok) {
      const error = new Error(response.statusText);
      let data = null;
      try {
        data = await response.json()
      } catch (_e) {
        // ignore
      }
      if (data) {
        error.message = data.message;
      }
      (/** @type {any} */ (error)).status = response.status;
      throw error;
    }
    return response;
  } catch (err) {
    notify.showError(err.message || 'Error performing resource action')
    throw err
  }
}

export async function putPublic(hash, path, content = '') {
  const result = await resourceActionPublic(hash, path, 'PUT', content)
  markDirMetadataItemChanged({ isShare: true, hash }, path)
  return result
}

export async function bulkDeletePublic(items) {
  if (!items || !Array.isArray(items) || items.length === 0) {
    throw new Error('items array is required and must not be empty')
  }

  const hash = state.shareInfo.hash;
  if (!hash) {
    throw new Error('share hash is required')
  }

  const params = {
    hash: hash,
    sessionId: state.sessionId
  }
  const apiPath = getPublicApiPath("resources/bulk", params)
  const baseUrl = window.origin + apiPath

  try {
    const response = await fetch(baseUrl, {
      method: 'DELETE',
      headers: {
        'Content-Type': 'application/json',
        ...sharePublicAuthHeaders(hash),
      },
      credentials: 'same-origin',
      body: JSON.stringify(items),
    })

    const data = await response.json()

    if (response.status === 200 || response.status === 207) {
      for (const item of items) {
        if (response.status === 200) {
          removeDirMetadataItem({ isShare: true, hash }, item.path)
        } else {
          invalidateDirMetadataItem({ isShare: true, hash }, item.path)
        }
      }
      return data
    }

    const error = new Error(data.message || response.statusText)
    error.status = response.status
    throw error
  } catch (err) {
    if (err.status && err.status !== 200 && err.status !== 207) {
      return {
        succeeded: [],
        failed: items.map(item => ({
          source: item.source || '',
          path: item.path,
          message: err.message || 'Delete failed',
        })),
      }
    }
    throw err
  }
}

export async function moveCopyPublic(
  hash,
  items,
  action = 'copy',
  overwrite = false,
  rename = false
) {
  if (!items || !Array.isArray(items) || items.length === 0) {
    throw new Error('items array is required and must not be empty')
  }

  try {
    const requestBody = {
      items: items.map(item => ({
        fromPath: item.from,
        toPath: item.to
      })),
      action: action,
      overwrite: overwrite,
      rename: rename
    }

    const apiPath = getPublicApiPath('resources', { hash: hash })
    const response = await fetch(apiPath, {
      method: 'PATCH',
      headers: {
        'Content-Type': 'application/json',
        ...sharePublicAuthHeaders(hash),
      },
      body: JSON.stringify(requestBody),
    })

    const data = await response.json()

    if (response.status === 200 || response.status === 207) {
      for (const item of items) {
        if (response.status !== 200) {
          invalidateDirMetadataItem({ isShare: true, hash }, item.from)
          invalidateDirMetadataItem({ isShare: true, hash }, item.to)
          continue
        }
        if (action === 'move') {
          removeDirMetadataItem({ isShare: true, hash }, item.from)
        }
        if (rename) {
          invalidateDirMetadataItem({ isShare: true, hash }, item.to)
        } else {
          markDirMetadataItemChanged({ isShare: true, hash }, item.isDir ? `${item.to}/` : item.to)
        }
      }
      return data
    }

    const error = new Error(data.message || response.statusText)
    error.status = response.status
    if (data.failed) {
      error.failed = data.failed
    }
    if (data.succeeded) {
      error.succeeded = data.succeeded
    }
    throw error
  } catch (err) {
    if (err.status && err.status !== 200 && err.status !== 207) {
      console.error(err.message || 'Error moving/copying resources')
    }
    throw err
  }
}
