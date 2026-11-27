package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNativeDownloadProgress(t *testing.T) {
	root, source, owner := setupShareArchiveDownloadTest(t)
	d := shareArchiveContext(owner, "/shared")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download", wrapHandler(func(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
		return RawFilesHandler(w, r, d, source, []string{r.URL.Query().Get("file")})
	}))
	for _, method := range []string{"GET", "DELETE"} {
		mux.HandleFunc(method+" /progress/{id}", wrapHandler(func(w http.ResponseWriter, r *http.Request, _ *Context) (int, error) {
			return downloadProgressHandler(w, r, d)
		}))
	}
	server := httptest.NewServer(LoggingMiddleware(mux))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second

	request := func(t *testing.T, method, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodHead {
			req.Header.Set("X-Track-Download", "1")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	start := func(t *testing.T, file string) (*http.Response, string) {
		t.Helper()
		path := "/download?file=" + file
		head := request(t, http.MethodHead, path)
		id := head.Header.Get(downloadProgressHeader)
		if head.StatusCode != http.StatusOK || id == "" {
			t.Fatalf("missing native download ID: status %d", head.StatusCode)
		}
		data := request(t, http.MethodGet, path+"&downloadId="+id)
		if data.StatusCode != http.StatusOK || data.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("unexpected download response: %d %v", data.StatusCode, data.Header)
		}
		return data, "/progress/" + id
	}
	progress := func(t *testing.T, path string) downloadProgress {
		t.Helper()
		resp := request(t, http.MethodGet, path)
		var p downloadProgress
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("single file", func(t *testing.T) {
		data, path := start(t, "/shared/hello.txt")
		body, err := io.ReadAll(data.Body)
		if err != nil || string(body) != "share-zip-contents" {
			t.Fatalf("unexpected download body: %q, %v", body, err)
		}
		if p := progress(t, path); p.Status != "completed" || p.Bytes != int64(len(body)) || p.Total != p.Bytes {
			t.Fatalf("unexpected file progress: %+v", p)
		}
	})

	t.Run("archive", func(t *testing.T) {
		data, path := start(t, "/shared")
		body, err := io.ReadAll(data.Body)
		if err != nil || len(readZipFileNames(body)) != 1 {
			t.Fatalf("expected a valid ZIP, got %d bytes, err=%v", len(body), err)
		}
		if p := progress(t, path); p.Status != "completed" || p.Bytes != int64(len(body)) || p.Total != 0 {
			t.Fatalf("unexpected archive progress: %+v", p)
		}
	})

	t.Run("cancel single file", func(t *testing.T) {
		const size = 512 << 10
		if err := os.WriteFile(filepath.Join(root, "shared", "large.txt"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		d.Share.MaxBandwidth = 64
		data, path := start(t, "/shared/large.txt")
		if p := progress(t, path); p.Status != "downloading" || p.Bytes <= 0 || p.Bytes >= size || p.Total != size {
			t.Fatalf("expected live progress, got %+v", p)
		}
		if resp := request(t, http.MethodDelete, path); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("cancel returned %d", resp.StatusCode)
		}
		body, err := io.ReadAll(data.Body)
		if err == nil || len(body) >= size {
			t.Fatalf("cancel did not interrupt the browser response: %d bytes, err=%v", len(body), err)
		}
		if p := progress(t, path); p.Status != "cancelled" {
			t.Fatalf("unexpected cancelled progress: %+v", p)
		}
		retry := request(t, http.MethodGet, data.Request.URL.RequestURI())
		if retry.StatusCode != http.StatusGone {
			t.Fatalf("cancelled download was restarted: %d", retry.StatusCode)
		}
	})

	t.Run("resume interrupted single file", func(t *testing.T) {
		const size = 512 << 10
		const remaining = 4096
		if err := os.WriteFile(filepath.Join(root, "shared", "resume.txt"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		d.Share.MaxBandwidth = 64
		data, path := start(t, "/shared/resume.txt")
		if err := data.Body.Close(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(2 * time.Second); ; {
			p := progress(t, path)
			if p.Status == "cancelled" || p.Status == "error" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("interrupted transfer did not stop: %+v", p)
			}
			time.Sleep(10 * time.Millisecond)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, data.Request.URL.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes="+strconv.Itoa(size-remaining)+"-")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusPartialContent || err != nil || len(body) != remaining {
			t.Fatalf("resume failed: status %d, %d bytes, err=%v", resp.StatusCode, len(body), err)
		}
		if p := progress(t, path); p.Status != "completed" || p.Bytes != size || p.Total != size {
			t.Fatalf("unexpected resumed progress: %+v", p)
		}
	})
}
