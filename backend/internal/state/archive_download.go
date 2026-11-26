package state

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gtsteffaniak/filebrowser/backend/internal/database/sqldb"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
)

const archiveDownloadLimitKey = "archive.downloadLimitGiB"

// ArchiveDownloadLimit uses server.maxArchiveSize until an admin saves a download limit.
func ArchiveDownloadLimit() (int64, error) {
	configLimit := settings.Config.Server.MaxArchiveSizeGB
	if sqlDb == nil {
		return configLimit, nil
	}
	raw, err := sqlDb.GetSetting(archiveDownloadLimitKey)
	if errors.Is(err, sqldb.ErrSettingNotFound) {
		return configLimit, nil
	}
	if err != nil {
		return 0, fmt.Errorf("load archive download limit: %w", err)
	}

	var limit int64
	if err := json.Unmarshal(raw, &limit); err != nil {
		return 0, fmt.Errorf("decode archive download limit: %w", err)
	}
	return limit, nil
}

func SaveArchiveDownloadLimit(limit int64) error {
	return sqlDb.SaveSetting(archiveDownloadLimitKey, limit)
}
