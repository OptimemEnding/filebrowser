package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/state"
	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/settings"
)

func TestArchiveCreate_DeniesResolvedSymlinkDestination(t *testing.T) {
	_, sourceRoot, sourceName, username := setupExtractAccessPolicyTest(t)
	if err := state.DenyUser(sourceRoot, utils.IndexPathFromNormalized("/denied", true), username); err != nil {
		t.Fatal(err)
	}
	allowedDir := filepath.Join(sourceRoot, "allowed")
	deniedDir := filepath.Join(sourceRoot, "denied")
	for _, dir := range []string{allowedDir, deniedDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(deniedDir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(allowedDir, "report.zip")
	if err := os.Symlink(filepath.Join("..", "denied", "victim"), link); err != nil {
		t.Fatal(err)
	}

	user, err := state.GetUserByUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	settings.Config.Server.SourceMap[sourceRoot].Config.DenyByDefault = false

	body := `{"fromSource":"` + sourceName + `","paths":["/a.txt"],"destination":"/allowed/report.zip","format":"zip"}`
	req := httptest.NewRequest(http.MethodPost, "/api/resources/archive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	status, handlerErr := archiveCreateHandler(rec, req, &Context{User: &user})
	if status != http.StatusForbidden || handlerErr == nil {
		t.Fatalf("status=%d err=%v", status, handlerErr)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "keep" {
		t.Fatalf("victim content=%q err=%v", got, err)
	}
}

func TestArchiveCreate_AllowsNonSymlinkDestination(t *testing.T) {
	_, sourceRoot, sourceName, username := setupExtractAccessPolicyTest(t)
	dest := filepath.Join(sourceRoot, "out.zip")
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	user, err := state.GetUserByUsername(username)
	if err != nil {
		t.Fatal(err)
	}

	body := `{"fromSource":"` + sourceName + `","paths":["/a.txt"],"destination":"/out.zip","format":"zip"}`
	req := httptest.NewRequest(http.MethodPost, "/api/resources/archive", strings.NewReader(body))
	rec := httptest.NewRecorder()
	status, handlerErr := archiveCreateHandler(rec, req, &Context{User: &user})
	if status != http.StatusOK || handlerErr != nil {
		t.Fatalf("status=%d err=%v body=%s", status, handlerErr, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected archive at %s: %v", dest, err)
	}
}

func TestArchiveSettingsLimitsApplyIndependently(t *testing.T) {
	_, root, sourceName, username := setupExtractAccessPolicyTest(t)
	settings.Config.Server.MaxArchiveSizeGB = 2
	for _, name := range []string{"large.txt", "large.zip"} {
		file, err := os.Create(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(1<<30 + 1); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	user, err := state.GetUserByUsername(username)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &Context{User: &user}
	const output = "/dest/output.zip"
	operations := []struct {
		name    string
		handler handleFunc
		method  string
		url     string
		body    string
		limits  string
	}{
		{"create", archiveCreateHandler, http.MethodPost, "/api/resources/archive",
			`{"fromSource":"` + sourceName + `","paths":["/large.txt"],"destination":"` + output + `"}`, `{"downloadGiB":5,"createGiB":1,"extractGiB":5}`},
		{"extract", unarchiveHandler, http.MethodPost, "/api/resources/unarchive",
			`{"fromSource":"` + sourceName + `","path":"/large.zip","destination":"/dest"}`, `{"downloadGiB":5,"createGiB":5,"extractGiB":1}`},
		{"download", downloadHandler, http.MethodHead,
			"/api/resources/download?source=" + sourceName + "&file=/large.txt&file=/large.zip&algo=zip", "", `{"downloadGiB":1,"createGiB":5,"extractGiB":5}`},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			settingsReq := httptest.NewRequest(http.MethodPut, "/api/settings/archive", strings.NewReader(op.limits))
			status, err := archiveSettingsPut(httptest.NewRecorder(), settingsReq, ctx)
			if status != http.StatusOK || err != nil {
				t.Fatalf("save archive limits: status=%d err=%v", status, err)
			}
			requestCtx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			req := httptest.NewRequestWithContext(requestCtx, op.method, op.url, strings.NewReader(op.body))
			status, err = op.handler(httptest.NewRecorder(), req, ctx)
			if status != http.StatusRequestEntityTooLarge || err == nil {
				t.Fatalf("expected saved limit to reject the request: status=%d err=%v", status, err)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, output)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("limit rejection should not create an archive: %v", err)
	}
}
