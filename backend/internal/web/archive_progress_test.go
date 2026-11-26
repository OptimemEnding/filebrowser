package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveProgressTracksTransferAndCancels(t *testing.T) {
	for _, cancelTransfer := range []bool{false, true} {
		name := "complete"
		if cancelTransfer {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			root, source, owner := setupShareArchiveDownloadTest(t)
			d := shareArchiveContext(owner, "/shared")
			if cancelTransfer {
				d.Share.MaxBandwidth = 64
				file := filepath.Join(root, "shared", "large.txt")
				if err := os.WriteFile(file, make([]byte, 512<<10), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			mux := http.NewServeMux()
			archiveHandler := func(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
				return BuildAndStreamArchive(w, r, d, source, []string{"/shared"})
			}
			mux.HandleFunc("GET /download", wrapHandler(archiveHandler))
			for _, method := range []string{"GET", "DELETE"} {
				mux.HandleFunc(method+" /progress/{id}", wrapHandler(func(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
					return archiveDownloadProgressHandler(w, r, d)
				}))
			}
			server := httptest.NewServer(LoggingMiddleware(mux))
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 5 * time.Second

			do := func(method, path string, track bool) *http.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				if track {
					req.Header.Set("X-Track-Archive", "1")
				}
				res, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				return res
			}
			head := do(http.MethodHead, "/download", true)
			_ = head.Body.Close()
			id := head.Header.Get(archiveProgressHeader)
			if head.StatusCode != http.StatusOK || id == "" {
				t.Fatalf("missing archive ID: status %d", head.StatusCode)
			}
			progressPath := "/progress/" + id
			progress := func() archiveDownloadProgress {
				t.Helper()
				res := do(http.MethodGet, progressPath, false)
				defer res.Body.Close()
				var p archiveDownloadProgress
				if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
					t.Fatal(err)
				}
				return p
			}
			if p := progress(); p.Status != "pending" || p.Bytes != 0 {
				t.Fatalf("unexpected initial progress: %+v", p)
			}
			data := do(http.MethodGet, "/download?archiveDownload="+id, false)
			defer data.Body.Close()

			if cancelTransfer {
				if p := progress(); p.Status != "downloading" || p.Bytes <= 0 {
					t.Fatalf("expected live byte count, got %+v", p)
				}
				res := do(http.MethodDelete, progressPath, false)
				_ = res.Body.Close()
				if res.StatusCode != http.StatusNoContent {
					t.Fatalf("cancel returned %d", res.StatusCode)
				}
				body, err := io.ReadAll(data.Body)
				if err == nil || len(body) >= 512<<10 {
					t.Fatalf("cancel failed to interrupt archive: %d bytes, err=%v", len(body), err)
				}
				if p := progress(); p.Status != "cancelled" {
					t.Fatalf("unexpected cancelled progress: %+v", p)
				}
				return
			}

			body, err := io.ReadAll(data.Body)
			if err != nil || len(readZipFileNames(body)) != 1 {
				t.Fatalf("expected a valid archive, got %d bytes, err=%v", len(body), err)
			}
			if p := progress(); p.Status != "completed" || p.Bytes != int64(len(body)) {
				t.Fatalf("unexpected final progress: %+v, body size %d", p, len(body))
			}
		})
	}
}
