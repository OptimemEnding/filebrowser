<template>
  <div class="card-content">
    <div class="settings-items">
      <ToggleSwitch class="item" v-model="localuser.disableUpdateNotifications" @change="updateSettings"
        :name="$t('profileSettings.disableUpdateNotifications')"
        :description="$t('profileSettings.disableUpdateNotificationsDescription')" />
      <ToggleSwitch class="item" v-model="analyticsEnabled" :disabled="analyticsLoading || !publishSupported"
        @update:model-value="updateAnalytics" :name="$t('settings.analyticsEnabled')"
        :description="$t('settings.analyticsEnabledDescription')" />
      <SettingsButton class="item" :name="$t('settings.analyticsView')"
        :description="$t('settings.analyticsViewDescription')" @activate="openAnalyticsPrompt" />
      <SettingsButton class="item" :name="$t('settings.configViewerOpen')"
        :description="$t('settings.configViewerDescription')" @activate="openConfigViewerPrompt" />
      <form class="item archive-download-setting" @submit.prevent="saveArchiveLimit">
        <div>
          <label for="archive-download-limit">{{ $t("fileLoading.archiveDownloadLimit") }}</label>
          <p id="archive-download-limit-help">{{ $t("fileLoading.archiveDownloadLimitHelp") }}</p>
        </div>
        <div class="archive-download-controls">
          <input id="archive-download-limit" v-model.number="archiveLimit" class="input"
            type="number" min="0" step="1" required aria-describedby="archive-download-limit-help"
            :disabled="savedArchiveLimit === null || savingArchiveLimit" />
          <button class="button" type="submit" :disabled="!canSaveArchiveLimit">
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
import { state, mutations } from "@/store/index.ts";
import {
  getAnalytics, updateAnalytics, getMediaSettings, getArchiveDownloadSettings, saveArchiveDownloadSettings,
} from "@/api/settings.js";
import { emptyMediaSettings, mergeMediaSettings, saveMediaSettingsPatch } from "@/utils/mediaSettings.js";
import ToggleSwitch from "@/components/settings/ToggleSwitch.vue";
import SettingsButton from "@/components/settings/SettingsButton.vue";

export default {
  name: "systemAdmin",
  components: {
    ToggleSwitch,
    SettingsButton,
  },
  data() {
    return {
      localuser: { disableUpdateNotifications: false },
      analyticsEnabled: false,
      publishSupported: false,
      analyticsLoading: false,
      archiveLimit: null,
      savedArchiveLimit: null,
      savingArchiveLimit: false,
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
    canSaveArchiveLimit() {
      return this.savedArchiveLimit !== null &&
        Number.isSafeInteger(this.archiveLimit) &&
        this.archiveLimit >= 0 &&
        this.archiveLimit !== this.savedArchiveLimit &&
        !this.savingArchiveLimit;
    },
    onlyOfficeDisabled() {
      return !this.mediaLoaded || this.savingOnlyOffice || this.isLocked("alwaysUseOnlyOffice");
    },
  },
  async mounted() {
    this.localuser = {
      disableUpdateNotifications: false,
      ...state.user,
    };
    await Promise.all([this.loadAnalytics(), this.loadMedia(), this.loadArchiveLimit()]);
  },
  methods: {
    async loadArchiveLimit() {
      try {
        const { maxSizeGiB } = await getArchiveDownloadSettings();
        this.archiveLimit = maxSizeGiB;
        this.savedArchiveLimit = maxSizeGiB;
      } catch (error) {
        notify.showError(error.message);
      }
    },
    async saveArchiveLimit() {
      if (!this.canSaveArchiveLimit) return;
      this.savingArchiveLimit = true;
      try {
        const { maxSizeGiB } = await saveArchiveDownloadSettings(this.archiveLimit);
        this.archiveLimit = maxSizeGiB;
        this.savedArchiveLimit = maxSizeGiB;
        notify.showSuccessToast(this.$t("settings.settingsUpdated"));
      } catch (error) {
        notify.showError(error.message);
      } finally {
        this.savingArchiveLimit = false;
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
    async updateSettings(event) {
      if (event && typeof event.preventDefault === "function") {
        event.preventDefault();
      }
      try {
        void mutations.updateCurrentUser(this.localuser);
        notify.showSuccessToast(this.$t("settings.settingsUpdated"));
      } catch (e) {
        console.error(e);
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

.archive-download-setting,
.archive-download-controls {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 0.75em;
}

.archive-download-setting {
  justify-content: space-between;
}

.archive-download-controls .input {
  width: 7em;
}
</style>
