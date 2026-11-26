package web

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
)

const (
	archiveProgressHeader = "X-Archive-Download-ID"
	archiveProgressTTL    = 5 * time.Minute
	maxArchiveDownloads   = 512
)

type archiveDownloadOwner struct {
	username string
	share    string
}

type archiveDownload struct {
	owner     archiveDownloadOwner
	status    string
	bytes     atomic.Int64
	cancel    context.CancelFunc
	expiresAt time.Time
}

var archiveDownloads = struct {
	sync.Mutex
	byID map[string]*archiveDownload
}{byID: make(map[string]*archiveDownload)}

func archiveOwner(d *Context) archiveDownloadOwner {
	return archiveDownloadOwner{username: d.User.Username, share: d.Share.Hash}
}

func prepareArchiveDownload(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	if r.Header.Get("X-Track-Archive") != "1" {
		return http.StatusOK, nil
	}
	id, err := utils.RandomHex(16)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	now := time.Now()
	archiveDownloads.Lock()
	defer archiveDownloads.Unlock()
	for key, download := range archiveDownloads.byID {
		if !download.expiresAt.IsZero() && !now.Before(download.expiresAt) {
			delete(archiveDownloads.byID, key)
		}
	}
	if len(archiveDownloads.byID) >= maxArchiveDownloads {
		return http.StatusServiceUnavailable, nil
	}
	archiveDownloads.byID[id] = &archiveDownload{
		owner: archiveOwner(d), status: "pending", expiresAt: now.Add(archiveProgressTTL),
	}
	w.Header().Set(archiveProgressHeader, id)
	w.Header().Set("Cache-Control", "no-store")
	return http.StatusOK, nil
}

// findArchiveDownload requires archiveDownloads to be locked.
func findArchiveDownload(id string, owner archiveDownloadOwner) *archiveDownload {
	download := archiveDownloads.byID[id]
	if download == nil || download.owner != owner {
		return nil
	}
	if !download.expiresAt.IsZero() && !time.Now().Before(download.expiresAt) {
		delete(archiveDownloads.byID, id)
		return nil
	}
	return download
}

func startArchiveDownload(r *http.Request, d *Context, id string) (*archiveDownload, context.Context, context.CancelFunc) {
	archiveDownloads.Lock()
	defer archiveDownloads.Unlock()
	download := findArchiveDownload(id, archiveOwner(d))
	if download == nil || download.status != "pending" {
		return nil, nil, nil
	}
	ctx, cancel := context.WithCancel(r.Context())
	download.cancel, download.status, download.expiresAt = cancel, "preparing", time.Time{}
	return download, ctx, cancel
}

func (d *archiveDownload) transferring() {
	archiveDownloads.Lock()
	defer archiveDownloads.Unlock()
	if d.status == "preparing" {
		d.status = "downloading"
	}
}

func (d *archiveDownload) finish(status string) {
	archiveDownloads.Lock()
	defer archiveDownloads.Unlock()
	if d.status == "preparing" || d.status == "downloading" {
		d.status = status
		d.expiresAt = time.Now().Add(archiveProgressTTL)
	}
	d.cancel = nil
}

type archiveDownloadProgress struct {
	Status string `json:"status"`
	Bytes  int64  `json:"bytes"`
}

func archiveDownloadProgressHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	archiveDownloads.Lock()
	download := findArchiveDownload(r.PathValue("id"), archiveOwner(d))
	if download == nil {
		archiveDownloads.Unlock()
		return http.StatusNotFound, nil
	}

	if r.Method == http.MethodDelete {
		var cancel context.CancelFunc
		switch download.status {
		case "pending", "preparing", "downloading":
			cancel = download.cancel
			download.cancel = nil
			download.status = "cancelled"
			download.expiresAt = time.Now().Add(archiveProgressTTL)
		}
		archiveDownloads.Unlock()
		if cancel != nil {
			cancel()
		}
		return http.StatusNoContent, nil
	}

	progress := archiveDownloadProgress{Status: download.status, Bytes: download.bytes.Load()}
	archiveDownloads.Unlock()
	return RenderJSON(w, r, progress)
}
