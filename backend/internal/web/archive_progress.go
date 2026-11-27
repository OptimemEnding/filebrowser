package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
)

const (
	archiveProgressHeader  = "X-Archive-Download-ID"
	downloadProgressHeader = "X-Download-ID"
	downloadProgressTTL    = 5 * time.Minute
	maxTrackedDownloads    = 512
)

type downloadOwner struct {
	username string
	share    string
}

type downloadTransfer struct {
	owner           downloadOwner
	status          string
	bytes           atomic.Int64
	total           atomic.Int64
	resumable       bool
	cancelRequested bool
	cancel          context.CancelFunc
	expiresAt       time.Time
}

var downloadTransfers = struct {
	sync.Mutex
	byID map[string]*downloadTransfer
}{byID: make(map[string]*downloadTransfer)}

func transferOwner(d *Context) downloadOwner {
	return downloadOwner{username: d.User.Username, share: d.Share.Hash}
}

func prepareArchiveDownload(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	if r.Header.Get("X-Track-Download") == "1" {
		return prepareDownloadTransfer(w, d, downloadProgressHeader, false)
	}
	if r.Header.Get("X-Track-Archive") != "1" {
		return http.StatusOK, nil
	}
	return prepareDownloadTransfer(w, d, archiveProgressHeader, false)
}

func prepareDownloadTransfer(w http.ResponseWriter, d *Context, header string, resumable bool) (int, error) {
	id, err := utils.RandomHex(16)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	now := time.Now()
	downloadTransfers.Lock()
	defer downloadTransfers.Unlock()
	for key, download := range downloadTransfers.byID {
		if !download.expiresAt.IsZero() && !now.Before(download.expiresAt) {
			delete(downloadTransfers.byID, key)
		}
	}
	if len(downloadTransfers.byID) >= maxTrackedDownloads {
		return http.StatusServiceUnavailable, nil
	}
	downloadTransfers.byID[id] = &downloadTransfer{
		owner: transferOwner(d), status: "pending", resumable: resumable, expiresAt: now.Add(downloadProgressTTL),
	}
	w.Header().Set(header, id)
	w.Header().Set("Cache-Control", "no-store")
	return http.StatusOK, nil
}

// findDownloadTransfer requires downloadTransfers to be locked.
func findDownloadTransfer(id string, owner downloadOwner) *downloadTransfer {
	download := downloadTransfers.byID[id]
	if download == nil || download.owner != owner {
		return nil
	}
	if !download.expiresAt.IsZero() && !time.Now().Before(download.expiresAt) {
		delete(downloadTransfers.byID, id)
		return nil
	}
	return download
}

func startDownloadTransfer(r *http.Request, d *Context, id string) (*downloadTransfer, context.Context, context.CancelFunc) {
	downloadTransfers.Lock()
	defer downloadTransfers.Unlock()
	download := findDownloadTransfer(id, transferOwner(d))
	if download == nil || download.cancelRequested {
		return nil, nil, nil
	}
	if download.status != "pending" {
		if !download.resumable || (download.status != "error" && download.status != "cancelled") {
			return nil, nil, nil
		}
		download.bytes.Store(0)
		download.total.Store(0)
	}
	ctx, cancel := context.WithCancel(r.Context())
	download.cancel, download.status, download.expiresAt = cancel, "preparing", time.Time{}
	return download, ctx, cancel
}

func bindDownloadTransfer(w http.ResponseWriter, r *http.Request, d *Context, id string) (*downloadTransfer, *http.Request, func(string)) {
	download, ctx, cancel := startDownloadTransfer(r, d, id)
	if download == nil {
		return nil, r, nil
	}
	controller := http.NewResponseController(w)
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = controller.SetWriteDeadline(time.Now())
	})
	return download, r.WithContext(ctx), func(status string) {
		if !stop() {
			<-interrupted
		}
		if ctx.Err() != nil {
			status = "cancelled"
		}
		download.finish(status)
		cancel()
	}
}

type downloadProgressWriter struct {
	ResponseWriterWrapper
	download *downloadTransfer
	expected int64
	written  int64
	writeErr error
}

func (w *downloadProgressWriter) WriteHeader(status int) {
	if w.WroteHeader {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if size, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64); err == nil && size >= 0 {
		w.expected = size
		w.download.total.Store(size)
	}
	if status == http.StatusPartialContent {
		var start, end, total int64
		if _, err := fmt.Sscanf(w.Header().Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err == nil {
			w.download.bytes.Store(start)
			w.download.total.Store(total)
		}
	}
	w.ResponseWriterWrapper.WriteHeader(status)
}

func (w *downloadProgressWriter) Write(p []byte) (int, error) {
	if !w.WroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriterWrapper.Write(p)
	if n > 0 && downloadResponseRecordsActivity(w.StatusCode, nil) {
		if w.written == 0 {
			w.download.transferring()
		}
		w.written += int64(n)
		w.download.bytes.Add(int64(n))
	}
	if err != nil {
		w.writeErr = err
	}
	return n, err
}

func trackDownloadResponse(w http.ResponseWriter, r *http.Request, d *Context, id string, serve func(http.ResponseWriter, *http.Request) (int, error)) (int, error) {
	download, r, finish := bindDownloadTransfer(w, r, d, id)
	if download == nil {
		return http.StatusGone, errors.New("download expired or already started")
	}
	status := "error"
	defer func() { finish(status) }()
	writer := &downloadProgressWriter{ResponseWriterWrapper: ResponseWriterWrapper{ResponseWriter: w}, download: download}
	code, err := serve(writer, r)
	if !downloadResponseRecordsActivity(code, err) {
		return code, err
	}
	err = writer.writeErr
	if err == nil && writer.expected > writer.written {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = http.NewResponseController(w).Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	if err != nil {
		if isClientDisconnect(r, err) {
			status = "cancelled"
		}
		panic(http.ErrAbortHandler)
	}
	status = "completed"
	return code, nil
}

func (d *downloadTransfer) transferring() {
	downloadTransfers.Lock()
	defer downloadTransfers.Unlock()
	if d.status == "preparing" {
		d.status = "downloading"
	}
}

func (d *downloadTransfer) finish(status string) {
	downloadTransfers.Lock()
	defer downloadTransfers.Unlock()
	if d.status == "preparing" || d.status == "downloading" {
		d.status = status
		d.expiresAt = time.Now().Add(downloadProgressTTL)
	}
	d.cancel = nil
}

type downloadProgress struct {
	Status string `json:"status"`
	Bytes  int64  `json:"bytes"`
	Total  int64  `json:"total,omitempty"`
}

func downloadProgressHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	downloadTransfers.Lock()
	download := findDownloadTransfer(r.PathValue("id"), transferOwner(d))
	if download == nil {
		downloadTransfers.Unlock()
		return http.StatusNotFound, nil
	}

	if r.Method == http.MethodDelete {
		var cancel context.CancelFunc
		switch download.status {
		case "pending", "preparing", "downloading":
			cancel = download.cancel
			download.cancel = nil
			download.cancelRequested = true
			download.status = "cancelled"
			download.expiresAt = time.Now().Add(downloadProgressTTL)
		}
		downloadTransfers.Unlock()
		if cancel != nil {
			cancel()
		}
		return http.StatusNoContent, nil
	}

	progress := downloadProgress{Status: download.status, Bytes: download.bytes.Load(), Total: download.total.Load()}
	downloadTransfers.Unlock()
	return RenderJSON(w, r, progress)
}
