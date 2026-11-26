<template>
  <div v-if="currentPrompt?.confirm" class="card-content">
    <p>{{ t("prompts.downloadMessage") }}</p>

    <button
      type="button"
      v-for="(ext, format) in formats"
      :key="format"
      class="button button--block"
      :aria-label="`Download as ${format}`"
      @click="handleFormatSelect(format)"
      v-focus
    >
      {{ ext }}
    </button>
  </div>
  <div v-if="!hasDownloads && (!currentPrompt?.confirm)" class="card-content lonely-message">
    <span>{{ t("files.lonely") }}</span>
  </div>
  <div v-if="hasDownloads && !currentPrompt?.confirm" class="card-content">
    <div ref="list" class="download-list">
      <div data-virtual-group="downloads" :style="virtual.group('downloads').style">
        <div
          v-for="{ download, index } in virtual.group('downloads').items"
          :key="download.id"
          class="download-item-wrapper"
          :class="{ 'has-error': download.status === 'error' }"
          :data-index="index"
        >
          <div class="download-item">
            <i class="material-symbols file-icon">insert_drive_file</i>
            <div class="file-info">
              <p class="file-name">{{ download.name }}</p>
              <div v-if="download.archiveRequest" class="archive-progress" role="status">
                <progress v-if="['pending', 'preparing', 'downloading'].includes(download.status)"
                  :aria-label="download.name" />
                <span>{{ archiveStatus(download) }}</span>
              </div>
              <ProgressBar v-else
                :val="download.status === 'completed'
                  ? t('prompts.completed')
                  : download.status === 'error'
                    ? t('prompts.error')
                    : download.loaded
                "
                :unit="download.status === 'completed' || download.status === 'error' ? '' : 'bytes'"
                :max="download.size"
                :status="download.status"
                text-position="inside"
                size="20">
              </ProgressBar>
            </div>
            <div class="file-actions">
              <button
                type="button"
                v-if="download.status === 'error'"
                @click="retryDownload(download.id)"
                class="action"
                :aria-label="t('general.retry')" :title="t('general.retry')"
              >
                <i class="material-symbols">replay</i>
              </button>
              <button
                type="button"
                @click="downloadManager.cancel(download.id)"
                class="action" :aria-label="t('general.cancel')"
                :title="t('general.cancel')"
              >
                <i class="material-symbols">close</i>
              </button>
            </div>
          </div>
          <div v-if="download.status === 'error'" class="error-banner" role="alert">
            {{ formatDownloadError(download) }}
          </div>
        </div>
      </div>
    </div>
  </div>
  <div class="card-actions">
    <div v-if="hasDownloads" class="spacer"></div>
    <button
      type="button"
      v-if="hasDownloads && hasClearable && !currentPrompt?.confirm"
      @click="downloadManager.clearCompleted()"
      class="button button--flat"
      :disabled="!hasClearable"
      :aria-label="t('buttons.clearCompleted')" :title="t('buttons.clearCompleted')"
    >
      {{ t("buttons.clearCompleted") }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed, useTemplateRef, watch } from "vue";
import { useI18n } from "@/i18n/index.ts";
import { getters, state } from "@/store/index.ts";
import { downloadManager } from "@/utils/downloadManager.ts";
import type { DownloadItem } from "@/utils/downloadManager.ts";
import { resourcesApi } from "@/api/index.js";
import ProgressBar from "@/components/ProgressBar.vue";
import { getHumanReadableFilesize } from "@/utils/filesizes.js";
import { findScrollParents, useListingVirtualization } from "@/composables/useListingVirtualization.ts";

const props = withDefaults(
  defineProps<{
    promptId?: string | number | null;
  }>(),
  { promptId: null },
);

defineOptions({ name: "download" });

const { t } = useI18n();

const vFocus = {
  mounted: (el: HTMLElement) => {
    setTimeout(() => el.focus(), 100);
  },
};

const formats = { zip: "zip", targz: "tar.gz" } as const;

const list = useTemplateRef<HTMLElement>("list");
const currentPrompt = computed(() => props.promptId == null
  ? getters.currentPrompt()
  : state.prompts.find((prompt) => prompt.id === props.promptId));
const downloads = computed(() => downloadManager.queue);
const hasDownloads = computed(() => downloads.value.length > 0);
const hasClearable = computed(() =>
  downloads.value.some(
    (download) => download.status === "completed" || download.status === "cancelled",
  ),
);

const virtual = useListingVirtualization<{ download: DownloadItem; index: number }>({
  container: list,
  groups: () => [
    {
      key: "downloads",
      count: downloads.value.length,
      at: (index) => ({ download: downloads.value[index] as DownloadItem, index }),
    },
  ],
  columnsHint: () => 1,
  scroller: (container) => findScrollParents(container, true),
  itemClass: "download-item-wrapper",
  minItems: 10,
});

watch(
  () => downloads.value.map((download) => download.status).join(),
  () => void virtual.refresh(),
);

function archiveStatus(download: DownloadItem): string {
  switch (download.status) {
    case "pending":
    case "preparing":
      return t("fileLoading.archiveDownloadPreparing");
    case "cancelled":
      return t("fileLoading.archiveDownloadCancelled");
    case "completed":
      return t("prompts.completed");
    case "error":
      return t("prompts.error");
    default:
      return t("fileLoading.archiveDownloadTransferred", { size: getHumanReadableFilesize(download.loaded) });
  }
}

function formatDownloadError(download: DownloadItem): string {
  if (download.status !== "error") {
    return "";
  }
  const detail = download.errorDetails ? String(download.errorDetails) : t("prompts.error");
  return download.archiveRequest ? detail : t("prompts.downloadFailed", { message: detail });
}

function handleFormatSelect(format: string) {
  const confirm = currentPrompt.value?.confirm;
  if (typeof confirm === "function") {
    confirm(format);
  }
}

async function retryDownload(id: number) {
  const download = downloadManager.findById(id);
  if (!download) return;
  // Re-trigger download by calling the download function again
  downloadManager.remove(id);
  try {
    const archive = download.archiveRequest;
    await resourcesApi.download(archive?.format ?? null, archive?.files ?? [download.file], download.shareHash);
  } catch (err) {
    console.error("Retry download failed:", err);
  }
}
</script>

<style scoped>
.download-list {
  overflow-y: auto;
  padding-right: 0.5em;
  flex-grow: 1;
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.download-item-wrapper {
  margin-bottom: 0.5rem;
}

.archive-progress {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 0.5em;
  font-size: 0.875rem;
}

.archive-progress progress {
  accent-color: var(--primaryColor);
  width: 6em;
}

.download-item-wrapper.has-error {
  border-left: 3px solid var(--errorColor, #f44336);
  padding-left: 0.5rem;
}

.error-banner {
  margin-top: 0.5rem;
  padding: 0.5rem;
  background: var(--errorBackground, rgb(244 67 54 / 10%));
  color: var(--errorColor, #f44336);
  border-radius: 4px;
  font-size: 0.875rem;
  overflow-wrap: break-word;
}

.download-item {
  display: flex;
  align-items: center;
  padding: 0.5em 0;
}

.file-icon {
  margin-right: 0.5em;
  color: #999;
}

.file-info {
  flex-grow: 1;
}

.file-name {
  margin: 0;
  font-size: 0.9em;
}

.file-actions .action {
  background: none;
  border: none;
  cursor: pointer;
  padding: 0.2em;
}

.file-actions .action i {
  font-size: 1.2em;
}

.spacer {
  flex-grow: 1;
}

.lonely-message {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 100px;
  text-align: center;
}
</style>
