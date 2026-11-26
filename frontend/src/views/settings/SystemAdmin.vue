<template>
  <div class="card-content">
    <div class="settings-items">
      <ToggleSwitch class="item" v-model="analyticsEnabled" :disabled="analyticsLoading || !publishSupported"
        @update:model-value="updateAnalytics" :name="$t('settings.analyticsEnabled')"
        :description="$t('settings.analyticsEnabledDescription')" />
      <SettingsButton class="item" :name="$t('settings.analyticsView')"
        :description="$t('settings.analyticsViewDescription')" @activate="openAnalyticsPrompt" />
      <SettingsButton class="item" :name="$t('settings.configViewerOpen')"
        :description="$t('settings.configViewerDescription')" @activate="openConfigViewerPrompt" />
      <form class="archive-limit-form" @submit.prevent="saveArchiveLimits">
        <div v-for="field in archiveLimitFields" :key="field.key" class="settings-number-input item archive-limit-setting">
          <label :for="field.key">{{ $t(field.label) }}</label>
          <div class="archive-limit-controls">
            <input :id="field.key" v-model.number="archiveLimits[field.key]" class="input"
              type="number" min="0" step="1" required
              :disabled="savedArchiveLimits === null || savingArchiveLimits" />
            <HelpTooltipIcon :text="$t(field.help)" :aria-label="$t('general.help')" />
          </div>
        </div>
        <div class="card-actions">
          <button class="button" type="submit" :disabled="!canSaveArchiveLimits">
            {{ $t("general.save", { suffix: "" }) }}
          </button>
        </div>
      </form>
      <ToggleSwitch class="item" v-model="media.alwaysUseOnlyOffice"
        :disabled="onlyOfficeDisabled"
        :value-tooltip="isLocked('alwaysUseOnlyOffice') ? $t('settings.lockedFromConfig') : ''"
        :name="$t('media.alwaysUseOffice')"
        :description="$t('media.alwaysUseOfficeDescription')"
        @update:model-value="saveAlwaysUseOnlyOffice" />
      <SettingsButton v-if="mediaLoadFailed" class="item" :name="$t('general.retry', { suffix: '' })"
        :description="$t('media.updateFailed')" @activate="loadMedia" />
    </div>
  </div>
</template>

<script>
import { notify } from "@/notify/index.ts";
import { mutations } from "@/store/index.ts";
import {
  getAnalytics, updateAnalytics, getMediaSettings, getArchiveSettings, saveArchiveSettings,
} from "@/api/settings.js";
import { emptyMediaSettings, mergeMediaSettings, saveMediaSettingsPatch } from "@/utils/mediaSettings.js";
import ToggleSwitch from "@/components/settings/ToggleSwitch.vue";
import SettingsButton from "@/components/settings/SettingsButton.vue";
import HelpTooltipIcon from "@/components/HelpTooltipIcon.vue";

const archiveLimitFields = [
  { key: "downloadGiB", label: "fileLoading.archiveDownloadLimit", help: "fileLoading.archiveDownloadLimitHelp" },
  { key: "createGiB", label: "fileLoading.archiveCreateLimit", help: "fileLoading.archiveCreateLimitHelp" },
  { key: "extractGiB", label: "fileLoading.archiveExtractLimit", help: "fileLoading.archiveExtractLimitHelp" },
];

export default {
  name: "systemAdmin",
  components: {
    ToggleSwitch,
    SettingsButton,
    HelpTooltipIcon,
  },
  data() {
    return {
      analyticsEnabled: false,
      publishSupported: false,
      analyticsLoading: false,
      archiveLimitFields,
      archiveLimits: { downloadGiB: null, createGiB: null, extractGiB: null },
      savedArchiveLimits: null,
      savingArchiveLimits: false,
      media: emptyMediaSettings(),
      /** Media settings pinned in the config file cannot be changed here. */
      lockedFromConfigPaths: [],
      lockMessage: "",
      /** True only after media settings and lock metadata both loaded. */
      mediaLoaded: false,
      mediaLoadFailed: false,
      /** Last value the server confirmed. A failed or skipped save restores it. */
      confirmedOnlyOffice: false,
      savingOnlyOffice: false,
    };
  },
  computed: {
    canSaveArchiveLimits() {
      if (!this.savedArchiveLimits || this.savingArchiveLimits) return false;
      const keys = archiveLimitFields.map(({ key }) => key);
      return keys.every((key) => Number.isSafeInteger(this.archiveLimits[key]) && this.archiveLimits[key] >= 0)
        && keys.some((key) => this.archiveLimits[key] !== this.savedArchiveLimits[key]);
    },
    onlyOfficeDisabled() {
      return !this.mediaLoaded || this.savingOnlyOffice || this.isLocked("alwaysUseOnlyOffice");
    },
  },
  async mounted() {
    await Promise.all([this.loadAnalytics(), this.loadMedia(), this.loadArchiveLimits()]);
  },
  methods: {
    async loadArchiveLimits() {
      try {
        const limits = await getArchiveSettings();
        this.archiveLimits = { ...limits };
        this.savedArchiveLimits = limits;
      } catch (error) {
        notify.showError(error.message);
      }
    },
    async saveArchiveLimits() {
      if (!this.canSaveArchiveLimits) return;
      this.savingArchiveLimits = true;
      try {
        const limits = await saveArchiveSettings(this.archiveLimits);
        this.archiveLimits = { ...limits };
        this.savedArchiveLimits = limits;
        notify.showSuccessToast(this.$t("settings.settingsUpdated"));
      } catch (error) {
        notify.showError(error.message);
      } finally {
        this.savingArchiveLimits = false;
      }
    },
    async loadMedia() {
      this.mediaLoadFailed = false;
      try {
        const data = await getMediaSettings();
        this.media = mergeMediaSettings(data);
        this.lockedFromConfigPaths = Array.isArray(data?.lockedFromConfigPaths)
          ? data.lockedFromConfigPaths
          : [];
        this.lockMessage = typeof data?.lockMessage === "string" ? data.lockMessage : "";
        this.confirmedOnlyOffice = this.media.alwaysUseOnlyOffice;
        // Enable the control only after both parts loaded.
        this.mediaLoaded = true;
      } catch (e) {
        console.error(e);
        this.mediaLoaded = false;
        this.mediaLoadFailed = true;
        notify.showErrorToast(this.$t("media.updateFailed"));
      }
    },
    isLocked(path) {
      return this.lockedFromConfigPaths.includes(path);
    },
    async saveAlwaysUseOnlyOffice() {
      // Saves never overlap. A change that arrives during a save is reverted, so the
      // control never shows a value the server has not confirmed.
      if (this.savingOnlyOffice) {
        this.media.alwaysUseOnlyOffice = this.confirmedOnlyOffice;
        return;
      }
      this.savingOnlyOffice = true;
      try {
        const { skipped, media } = await saveMediaSettingsPatch(
          { alwaysUseOnlyOffice: this.media.alwaysUseOnlyOffice },
          this.lockedFromConfigPaths,
        );
        if (skipped) {
          this.media.alwaysUseOnlyOffice = this.confirmedOnlyOffice;
          notify.showSuccessToast(this.lockMessage || this.$t("settings.lockedFromConfig"));
          return;
        }
        this.media = media;
        this.confirmedOnlyOffice = media.alwaysUseOnlyOffice;
        void mutations.syncMediaSettings();
        notify.showSuccessToast(this.$t("settings.settingsUpdated"));
      } catch (e) {
        // v-model already changed the local value. Show the value the server still has.
        this.media.alwaysUseOnlyOffice = this.confirmedOnlyOffice;
        notify.showError(e?.message || this.$t("media.updateFailed"));
      } finally {
        this.savingOnlyOffice = false;
      }
    },
    async loadAnalytics() {
      try {
        const status = await getAnalytics();
        this.analyticsEnabled = status.enabled;
        this.publishSupported = status.publishSupported;
      } catch (e) {
        console.error(e);
        notify.showErrorToast(e?.message || this.$t("settings.analyticsLoadFailed"));
      }
    },
    async updateAnalytics() {
      if (!this.publishSupported) {
        this.analyticsEnabled = false;
        return;
      }
      this.analyticsLoading = true;
      try {
        const status = await updateAnalytics({ enabled: this.analyticsEnabled });
        this.analyticsEnabled = status.enabled;
        notify.showSuccessToast(this.$t("settings.settingsUpdated"));
      } catch (e) {
        console.error(e);
        notify.showErrorToast(e?.message || this.$t("settings.analyticsUpdateFailed"));
        await this.loadAnalytics();
      } finally {
        this.analyticsLoading = false;
      }
    },
    openAnalyticsPrompt() {
      mutations.showPrompt({
        name: "analytics-diagnostic",
        props: {
          title: this.$t("settings.analyticsView"),
        },
      });
    },
    openConfigViewerPrompt() {
      mutations.showPrompt({
        name: "config-viewer",
        props: {
          title: this.$t("settings.configViewer"),
        },
      });
    },
  },
};
</script>

<style scoped>
.card-content {
  margin-top: 1em;
}

.archive-limit-form {
  display: grid;
  gap: 0.75em;
}

.archive-limit-setting,
.archive-limit-controls {
  display: flex;
  gap: 0.75em;
}

.archive-limit-setting {
  align-items: center;
  justify-content: space-between;
}

.archive-limit-controls {
  align-items: flex-start;
}

.archive-limit-controls .input {
  width: 7em;
}
</style>
