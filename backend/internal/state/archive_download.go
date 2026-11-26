package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/gtsteffaniak/filebrowser/backend/internal/database/sqldb"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
)

const (
	archiveLimitsKey              = "archive.limitsGiB"
	legacyArchiveDownloadLimitKey = "archive.downloadLimitGiB"
)

var archiveLimitsMu sync.Mutex

type ArchiveLimits struct {
	DownloadGiB int64 `json:"downloadGiB"`
	CreateGiB   int64 `json:"createGiB"`
	ExtractGiB  int64 `json:"extractGiB"`
}

func GetArchiveLimits() (ArchiveLimits, error) {
	archiveLimitsMu.Lock()
	defer archiveLimitsMu.Unlock()
	return getArchiveLimits()
}

func getArchiveLimits() (ArchiveLimits, error) {
	fallback := settings.Config.Server.MaxArchiveSizeGB
	limits := ArchiveLimits{DownloadGiB: fallback, CreateGiB: fallback, ExtractGiB: fallback}
	if sqlDb == nil {
		return limits, nil
	}

	raw, err := sqlDb.GetSetting(archiveLimitsKey)
	if err == nil {
		if err := json.Unmarshal(raw, &limits); err != nil {
			return limits, fmt.Errorf("decode archive limits: %w", err)
		}
		return limits, nil
	}
	if !errors.Is(err, sqldb.ErrSettingNotFound) {
		return limits, fmt.Errorf("load archive limits: %w", err)
	}

	raw, err = sqlDb.GetSetting(legacyArchiveDownloadLimitKey)
	if errors.Is(err, sqldb.ErrSettingNotFound) {
		return limits, nil
	}
	if err != nil {
		return limits, fmt.Errorf("load existing download limit: %w", err)
	}
	if err := json.Unmarshal(raw, &limits.DownloadGiB); err != nil {
		return limits, fmt.Errorf("decode existing download limit: %w", err)
	}
	return limits, nil
}

func SaveArchiveLimits(limits ArchiveLimits) error {
	archiveLimitsMu.Lock()
	defer archiveLimitsMu.Unlock()
	return sqlDb.SaveSetting(archiveLimitsKey, limits)
}

func SaveArchiveDownloadLimit(limit int64) error {
	archiveLimitsMu.Lock()
	defer archiveLimitsMu.Unlock()
	limits, err := getArchiveLimits()
	if err != nil {
		return err
	}
	limits.DownloadGiB = limit
	return sqlDb.SaveSetting(archiveLimitsKey, limits)
}
