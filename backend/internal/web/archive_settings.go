package web

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gtsteffaniak/filebrowser/backend/internal/state"
)

func archiveSettingsGet(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	limits, err := state.GetArchiveLimits()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, limits)
}

func archiveSettingsPut(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	body, err := readSettingsPatchBody(w, r)
	if err != nil {
		return http.StatusBadRequest, err
	}
	var input struct {
		DownloadGiB *int64 `json:"downloadGiB"`
		CreateGiB   *int64 `json:"createGiB"`
		ExtractGiB  *int64 `json:"extractGiB"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid archive settings: %w", err)
	}
	for _, value := range []*int64{input.DownloadGiB, input.CreateGiB, input.ExtractGiB} {
		if value == nil || *value < 0 {
			return http.StatusBadRequest, fmt.Errorf("each archive limit must be a non-negative integer")
		}
	}
	limits := state.ArchiveLimits{
		DownloadGiB: *input.DownloadGiB, CreateGiB: *input.CreateGiB, ExtractGiB: *input.ExtractGiB,
	}
	if err := state.SaveArchiveLimits(limits); err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, limits)
}

// The previous download-only endpoint remains compatible with cached frontend assets.
func archiveDownloadSettingsGet(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	limits, err := state.GetArchiveLimits()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, map[string]int64{"maxSizeGiB": limits.DownloadGiB})
}

func archiveDownloadSettingsPut(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
	body, err := readSettingsPatchBody(w, r)
	if err != nil {
		return http.StatusBadRequest, err
	}
	var input struct {
		MaxSizeGiB *int64 `json:"maxSizeGiB"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid archive settings: %w", err)
	}
	if input.MaxSizeGiB == nil || *input.MaxSizeGiB < 0 {
		return http.StatusBadRequest, fmt.Errorf("maxSizeGiB must be a non-negative integer")
	}
	if err := state.SaveArchiveDownloadLimit(*input.MaxSizeGiB); err != nil {
		return http.StatusInternalServerError, err
	}
	return RenderJSON(w, r, input)
}
