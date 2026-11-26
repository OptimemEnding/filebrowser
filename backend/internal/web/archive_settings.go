package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gtsteffaniak/filebrowser/backend/internal/state"
)

type archiveDownloadSettings struct {
	MaxSizeGiB *int64 `json:"maxSizeGiB"`
}

func archiveDownloadSettingsGet(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	limit, err := state.ArchiveDownloadLimit()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, archiveDownloadSettings{MaxSizeGiB: &limit})
}

func archiveDownloadSettingsPut(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	body, err := readSettingsPatchBody(w, r)
	if err != nil {
		return http.StatusBadRequest, err
	}
	var prefs archiveDownloadSettings
	if err := json.Unmarshal(body, &prefs); err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid archive download settings: %w", err)
	}
	if prefs.MaxSizeGiB == nil || *prefs.MaxSizeGiB < 0 {
		return http.StatusBadRequest, fmt.Errorf("maxSizeGiB must be a non-negative integer")
	}
	if err := state.SaveArchiveDownloadLimit(*prefs.MaxSizeGiB); err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, prefs)
}
