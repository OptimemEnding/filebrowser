package web

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/internal/activity"
	"github.com/gtsteffaniak/filebrowser/backend/internal/adapters/fs/files"
	"github.com/gtsteffaniak/filebrowser/backend/internal/adapters/fs/fileutils"
	"github.com/gtsteffaniak/filebrowser/backend/internal/archiveencoding"
	activitydb "github.com/gtsteffaniak/filebrowser/backend/internal/database/activity"
	"github.com/gtsteffaniak/filebrowser/backend/internal/state"
	"github.com/gtsteffaniak/filebrowser/backend/internal/utils"
	"github.com/gtsteffaniak/filebrowser/backend/pkg/indexing"
	"github.com/gtsteffaniak/go-logger/logger"
	"golang.org/x/time/rate"
)

// archiveAttachmentStem is the filename stem (no extension) for Content-Disposition on multi-item /
// directory downloads. It prefers the client file path so names match the UI (e.g. "Desktop.zip"); the
// old filepath.Dir(realPath)+Base trick can yield "/" at filesystem root or the wrong parent if index metadata lags.
func archiveAttachmentStem(fileList []string, realPath string) string {
	if len(fileList) == 0 {
		return "download"
	}
	first := filepath.ToSlash(strings.TrimSuffix(strings.TrimSpace(fileList[0]), "/"))
	var stem string
	if len(fileList) == 1 {
		stem = path.Base(first)
	} else {
		stem = path.Base(path.Dir(first))
	}
	if stem == "" || stem == "." || stem == "/" {
		if len(fileList) == 1 {
			stem = filepath.Base(realPath)
		} else {
			stem = filepath.Base(filepath.Dir(realPath))
		}
	}
	if stem == "" || stem == "." || stem == "/" {
		return "download"
	}
	return stem
}

// archiveCreateHandler creates an archive on the server at the given destination.
// POST /resources/archive — server-side only; does not return archive data.
//
// @Summary Create an archive on the server
// @Description Creates a zip or tar.gz archive on the server from the given paths (files and/or directories). Server-side only; no archive bytes are returned. All items must be from the same source. Folders are walked recursively; access-denied paths are silently skipped. Requires create permission. Archive size is checked against the archive size limit if configured.
// @Description
// @Description **Request body parameters:**
// @Description - **fromSource** (string, required): Source name where the paths to archive live. Example: `"default"`
// @Description - **toSource** (string, optional): Source name where the archive file will be written. Defaults to fromSource if omitted. Example: `"backups"`
// @Description - **paths** (array of strings, required): Paths of files or directories to add to the archive (relative to fromSource). Directories are walked; access-denied entries are skipped. Example: `["/docs/file.txt", "/photos"]`
// @Description - **destination** (string, required): Full path where the archive file will be created (on toSource). Must end with .zip or .tar.gz (or format is inferred). Example: `"/backups/my-archive.zip"`
// @Description - **format** (string, optional): Archive format. One of: `"zip"`, `"tar.gz"`. Default inferred from destination extension. Example: `"zip"`
// @Description - **compression** (integer, optional): Gzip compression level for tar.gz only (0–9). 0 = default. Ignored for zip. Example: `6`
// @Description - **deleteAfter** (boolean, optional): If true, delete source files/directories after successful creation. Requires delete permission. Example: `true`
// @Tags Resources
// @Accept json
// @Produce json
// @Param body body archiveCreateRequest true "Request body: fromSource, toSource (optional), paths, destination, format (optional), compression (optional)"
// @Success 200 {object} map[string]string "Created; returns {\"path\": \"<destination path>\"}"
// @Failure 400 {object} map[string]string "Invalid request (e.g. missing required field, invalid path)"
// @Failure 403 {object} map[string]string "Forbidden (create permission or access denied)"
// @Failure 404 {object} map[string]string "Source not found"
// @Failure 413 {object} map[string]string "Request Entity Too Large (archive exceeds the configured size limit)"
// @Failure 500 {object} map[string]string "Internal server error"
// @Router /api/resources/archive [post]
func archiveCreateHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	if d.Share.Hash != "" {
		return http.StatusForbidden, fmt.Errorf("archive create not allowed for shares")
	}

	var req archiveCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}
	if req.FromSource == "" || len(req.Paths) == 0 || req.Destination == "" {
		return http.StatusBadRequest, fmt.Errorf("fromSource, paths, and destination are required")
	}

	destClean, err := utils.SanitizePath(req.Destination)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid destination path: %v", err)
	}
	req.Destination = destClean
	pathsClean := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		var clean string
		clean, err = utils.SanitizePath(p)
		if err != nil {
			return http.StatusBadRequest, fmt.Errorf("invalid path %q: %v", p, err)
		}
		pathsClean = append(pathsClean, clean)
	}
	req.Paths = pathsClean

	destSource := req.ToSource
	if destSource == "" {
		destSource = req.FromSource
	}
	if status, roErr := denyIfSourceReadOnly(destSource); roErr != nil {
		return status, roErr
	}
	if status, roErr := denyIfSourceReadOnly(req.FromSource); req.DeleteAfter && roErr != nil {
		return status, roErr
	}
	destPerms, err := effectiveFilePerms(d, destSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	if !destPerms.Create {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to create resources in destination source")
	}
	fromPerms, err := effectiveFilePerms(d, req.FromSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	if !fromPerms.Download {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to download source files")
	}
	if req.DeleteAfter && !fromPerms.Delete {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to delete source files")
	}

	idx := indexing.GetIndex(req.FromSource)
	if idx == nil {
		return http.StatusNotFound, fmt.Errorf("source %s not found", req.FromSource)
	}
	userScope, err := d.User.GetScopeForSourceName(req.FromSource)
	if err != nil {
		return http.StatusForbidden, err
	}

	// Resolve destination on ToSource (or Source if not set)
	idxTo := indexing.GetIndex(destSource)
	if idxTo == nil {
		return http.StatusNotFound, fmt.Errorf("source %s not found", destSource)
	}
	userScopeTo, err := d.User.GetScopeForSourceName(destSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	fullDestFile := utils.JoinPathAsUnix(userScopeTo, req.Destination)
	if !state.AccessPermitted(idxTo.Path, utils.IndexPathFromNormalized(fullDestFile, true), d.User.Username) {
		return http.StatusForbidden, fmt.Errorf("access denied to destination %s", req.Destination)
	}
	fullDestParent := utils.GetParentDirectoryPath(fullDestFile)
	if fullDestParent != fullDestFile && !state.AccessPermitted(idxTo.Path, utils.IndexPathFromNormalized(fullDestParent, true), d.User.Username) {
		return http.StatusForbidden, fmt.Errorf("access denied to destination parent of %s", req.Destination)
	}
	destRel := strings.TrimLeft(filepath.ToSlash(req.Destination), "/")
	fileName := path.Base(destRel)
	if fileName == "." || fileName == "/" {
		return http.StatusBadRequest, fmt.Errorf("invalid destination file name")
	}
	destFileReal, err := idxTo.GetRealPathForMutation(userScopeTo, fullDestFile)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("destination path invalid: %v", err)
	}
	if !state.AccessPermittedAtRealPath(idxTo, d.User.Username, destFileReal, false) {
		return http.StatusForbidden, fmt.Errorf("access denied to destination %s", req.Destination)
	}
	if err = os.MkdirAll(filepath.Dir(destFileReal), fileutils.EffectiveDirPerm()); err != nil {
		return http.StatusInternalServerError, fmt.Errorf("cannot create destination parent directory: %v", err)
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		destLower := strings.ToLower(req.Destination)
		if strings.HasSuffix(destLower, ".tar.gz") {
			format = "tar.gz"
		} else if strings.HasSuffix(destLower, ".zip") || filepath.Ext(req.Destination) == ".zip" {
			format = "zip"
		} else {
			format = "zip"
		}
	}
	if format != "zip" && format != "tar.gz" {
		return http.StatusBadRequest, fmt.Errorf("format must be zip or tar.gz")
	}

	compression := req.Compression
	if compression < 0 || compression > 9 {
		compression = 0
	}

	// Build full paths for items (same source)
	itemPaths := make([]string, 0, len(req.Paths))
	for _, it := range req.Paths {
		full := utils.JoinPathAsUnix(userScope, it)
		if !state.AccessPermitted(idx.Path, utils.IndexPathFromNormalized(full, true), d.User.Username) {
			continue // silently skip
		}
		itemPaths = append(itemPaths, full)
	}
	if len(itemPaths) == 0 {
		return http.StatusBadRequest, fmt.Errorf("no paths accessible; add at least one path you have access to")
	}

	limits, err := state.GetArchiveLimits()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if status, err := checkArchiveInputSize(r.Context(), req.FromSource, itemPaths, d, limits.CreateGiB); err != nil {
		return status, err
	}

	var createErr error
	file, err := os.OpenFile(destFileReal, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileutils.EffectiveFilePerm())
	if err != nil {
		return http.StatusInternalServerError, err
	}
	defer file.Close()

	if format == "zip" {
		createErr = createZip(r.Context(), d, req.FromSource, file, itemPaths...)
	} else {
		createErr = createTarGzWithLevel(r.Context(), d, req.FromSource, file, compression, itemPaths...)
	}
	if createErr != nil {
		return http.StatusInternalServerError, createErr
	}

	if req.DeleteAfter {
		type itemToDelete struct {
			indexPath string
			isDir     bool
		}
		var toDelete []itemToDelete
		for _, full := range itemPaths {
			_, isDir, err := idx.GetRealPath(full)
			if err != nil {
				continue
			}
			toDelete = append(toDelete, itemToDelete{indexPath: full, isDir: isDir})
		}
		for i := 0; i < len(toDelete); i++ {
			for j := i + 1; j < len(toDelete); j++ {
				if len(toDelete[j].indexPath) > len(toDelete[i].indexPath) {
					toDelete[i], toDelete[j] = toDelete[j], toDelete[i]
				}
			}
		}
		for _, item := range toDelete {
			if err := files.DeleteFiles(req.FromSource, userScope, item.indexPath, item.isDir); err != nil {
				logger.Errorf("Failed to delete source after archive: %v", err)
			}
		}
	}

	activity.RecordArchive(r, toActor(d), activitydb.EventArchive, req.FromSource, req.Destination, req.Paths)
	return RenderJSON(w, r, map[string]string{"path": req.Destination}, http.StatusOK)
}

// unarchiveHandler extracts an archive on the server. POST /resources/unarchive — server-side only.
//
// @Summary Extract an archive on the server
// @Description Extracts a zip or tar.gz archive on the server into the given destination directory. Server-side only; no extracted bytes are returned. Supports extracting to a different source via toSource. Requires create permission. Archive size is checked against the archive size limit if configured.
// @Description
// @Description **Request body parameters:**
// @Description - **fromSource** (string, required): Source name where the archive file lives. Example: `"default"`
// @Description - **toSource** (string, optional): Source name where contents will be extracted. Defaults to fromSource if omitted. Example: `"restored"`
// @Description - **path** (string, required): Path to the archive file (on fromSource). Must be .zip, .tar.gz, or .tgz. Example: `"/downloads/data.zip"`
// @Description - **destination** (string, required): Directory path (on toSource) to extract into. Example: `"/projects/imported"`
// @Description - **deleteAfter** (boolean, optional): If true, delete the archive file after successful extraction. Default: false. Example: `true`
// @Description - **filenameEncoding** (string, optional): Fallback ZIP filename encoding: utf-8, cp932, gb18030, big5, euc-kr, cp437, windows-1252. Unicode metadata takes precedence. Omit to use a suggestion; ambiguous encodings require a selection.
// @Description - **preview** (boolean, optional): Return ZIP encoding candidates and sample filenames without extracting or deleting anything. Requires the same source/destination permissions as extraction.
// @Tags Resources
// @Accept json
// @Produce json
// @Param body body unarchiveRequest true "Request body: fromSource, toSource (optional), path, destination, deleteAfter (optional)"
// @Success 200 {object} map[string]interface{} "Preview returns suggested and candidates; extraction returns path and source. Extracted; returns {\"path\": \"<destination path>\", \"source\": \"<toSource>\"}"
// @Failure 400 {object} map[string]string "Invalid request (e.g. missing required field, unsupported format)"
// @Failure 403 {object} map[string]string "Forbidden (create permission or access denied)"
// @Failure 404 {object} map[string]string "Source or archive file not found"
// @Failure 413 {object} map[string]string "Request Entity Too Large (archive exceeds the configured size limit)"
// @Failure 500 {object} map[string]string "Internal server error"
// @Router /api/resources/unarchive [post]
func unarchiveHandler(w http.ResponseWriter, r *http.Request, d *Context) (int, error) {
	if d.Share.Hash != "" {
		return http.StatusForbidden, fmt.Errorf("unarchive not allowed for shares")
	}

	var req unarchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err)
	}
	if req.FromSource == "" || req.Path == "" || req.Destination == "" {
		return http.StatusBadRequest, fmt.Errorf("fromSource, path, and destination are required")
	}
	if req.ToSource == "" {
		req.ToSource = req.FromSource
	}
	if !req.Preview {
		if status, roErr := denyIfSourceReadOnly(req.ToSource); roErr != nil {
			return status, roErr
		}
	}
	if status, roErr := denyIfSourceReadOnly(req.FromSource); req.DeleteAfter && !req.Preview && roErr != nil {
		return status, roErr
	}
	destPerms, err := effectiveFilePerms(d, req.ToSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	if !destPerms.Create {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to create resources in destination source")
	}
	fromPerms, err := effectiveFilePerms(d, req.FromSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	if !fromPerms.Download {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to download archive source")
	}
	if req.DeleteAfter && !req.Preview && !fromPerms.Delete {
		return http.StatusForbidden, fmt.Errorf("user is not allowed to delete archive source")
	}

	pathClean, err := utils.SanitizePath(req.Path)
	if err != nil {
		return http.StatusBadRequest, err
	}
	destClean, err := utils.SanitizePath(req.Destination)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("invalid destination: %v", err)
	}
	req.Path = pathClean
	req.Destination = destClean

	idxFrom := indexing.GetIndex(req.FromSource)
	if idxFrom == nil {
		return http.StatusNotFound, fmt.Errorf("source %s not found", req.FromSource)
	}
	userScopeFrom, err := d.User.GetScopeForSourceName(req.FromSource)
	if err != nil {
		return http.StatusForbidden, err
	}

	fullArchivePath := utils.JoinPathAsUnix(userScopeFrom, req.Path)
	if !state.AccessPermitted(idxFrom.Path, utils.IndexPathFromNormalized(fullArchivePath, true), d.User.Username) {
		return http.StatusForbidden, fmt.Errorf("access denied to archive %s", req.Path)
	}
	archiveReal, _, err := idxFrom.GetRealPathScoped(userScopeFrom, fullArchivePath)
	if err != nil {
		return http.StatusNotFound, fmt.Errorf("archive path not found: %v", err)
	}

	idxTo := indexing.GetIndex(req.ToSource)
	if idxTo == nil {
		return http.StatusNotFound, fmt.Errorf("source %s not found", req.ToSource)
	}
	userScopeTo, err := d.User.GetScopeForSourceName(req.ToSource)
	if err != nil {
		return http.StatusForbidden, err
	}
	fullDestPath := utils.JoinPathAsUnix(userScopeTo, req.Destination)
	if !state.AccessPermitted(idxTo.Path, utils.IndexPathFromNormalized(fullDestPath, true), d.User.Username) {
		return http.StatusForbidden, fmt.Errorf("access denied to destination %s", req.Destination)
	}
	destReal, _, err := idxTo.GetRealPathScoped(userScopeTo, fullDestPath)
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("destination path invalid: %v", err)
	}
	destInfo, err := os.Stat(destReal)
	if err != nil {
		if os.IsNotExist(err) {
			return http.StatusBadRequest, fmt.Errorf("destination directory does not exist: %s", req.Destination)
		}
		return http.StatusInternalServerError, fmt.Errorf("destination: %v", err)
	}
	if !destInfo.IsDir() {
		return http.StatusBadRequest, fmt.Errorf("destination must be a directory: %s", req.Destination)
	}

	info, err := os.Stat(archiveReal)
	if err != nil {
		return http.StatusNotFound, fmt.Errorf("archive not found: %v", err)
	}
	if info.IsDir() {
		return http.StatusBadRequest, fmt.Errorf("path is not an archive file: %s", req.Path)
	}

	limits, err := state.GetArchiveLimits()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if err := checkArchiveSizeLimit(info.Size(), limits.ExtractGiB); err != nil {
		return http.StatusRequestEntityTooLarge, err
	}

	lower := strings.ToLower(archiveReal)
	if req.Preview {
		if !strings.HasSuffix(lower, ".zip") {
			return http.StatusBadRequest, fmt.Errorf("encoding preview requires a ZIP archive")
		}
		zr, openErr := zip.OpenReader(archiveReal)
		if openErr != nil {
			return http.StatusBadRequest, openErr
		}
		defer zr.Close()
		return RenderJSON(w, r, archiveencoding.Inspect(zr.File), http.StatusOK)
	}
	policy := &extractAccessPolicy{
		idx:             idxTo,
		destRealRoot:    destReal,
		destIndexPrefix: fullDestPath,
		username:        d.User.Username,
		perms:           destPerms,
	}
	var extractErr error
	if strings.HasSuffix(lower, ".zip") {
		extractErr = extractZipWithEncoding(archiveReal, destReal, req.FilenameEncoding, policy)
	} else if strings.HasSuffix(lower, ".tar.gz") || (strings.HasSuffix(lower, ".tgz")) {
		extractErr = extractTarGz(archiveReal, destReal, policy)
	} else {
		return http.StatusBadRequest, fmt.Errorf("unsupported archive format (use .zip or .tar.gz)")
	}
	if extractErr != nil {
		if errors.Is(extractErr, archiveencoding.ErrEncoding) {
			return http.StatusBadRequest, extractErr
		}
		if errors.Is(extractErr, errExtractAccessDenied) {
			return http.StatusForbidden, extractErr
		}
		logger.Errorf("archive extraction failed: %v", extractErr)
		return http.StatusInternalServerError, clientExtractError()
	}

	if req.DeleteAfter {
		if err := os.Remove(archiveReal); err != nil {
			logger.Errorf("Failed to delete archive after extract: %v", err)
		}
	}

	activity.RecordArchive(r, toActor(d), activitydb.EventUnarchive, req.FromSource, req.Destination, []string{req.Path})
	return RenderJSON(w, r, map[string]string{"path": req.Destination, "source": req.ToSource}, http.StatusOK)
}

// walkArchiveEntries visits the accessible entries used by both archive creation and size checks.
func walkArchiveEntries(ctx context.Context, source, indexPath string, d *Context, visit func(string, string, fs.FileInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	idx := indexing.GetIndex(source)
	if idx == nil {
		return fmt.Errorf("source %s is not available", source)
	}
	permUser := accessCheckUsername(d)
	if !state.AccessPermitted(idx.Path, utils.IndexPathFromNormalized(indexPath, true), permUser) {
		return nil
	}

	bound := d.Share.Path
	if d.Share.Hash == "" {
		var err error
		bound, err = d.User.GetScopeForSourceName(source)
		if err != nil {
			return err
		}
	}
	realPath, _, err := idx.GetRealPathScoped(bound, indexPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return err
	}
	baseName := filepath.Base(realPath)
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return nil
		}
		return visit(realPath, baseName, info)
	}

	return filepath.WalkDir(realPath, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filePath == realPath {
			return nil
		}
		relPath, err := filepath.Rel(realPath, filePath)
		if err != nil {
			return err
		}

		childIndexPath := utils.JoinPathAsUnix(indexPath, filepath.ToSlash(relPath))
		// Apply the share owner's rules before resolving the entry.
		allowed := state.AccessPermitted(idx.Path, utils.IndexPathFromNormalized(childIndexPath, true), permUser)
		if !allowed {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		resolved, _, err := idx.GetRealPathScoped(bound, childIndexPath)
		if err != nil {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := os.Stat(resolved)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !(entry.IsDir() && info.IsDir()) {
			return nil
		}
		archivePath := filepath.ToSlash(filepath.Join(baseName, relPath))
		return visit(resolved, archivePath, info)
	})
}

func addFile(ctx context.Context, source, indexPath string, d *Context, tarWriter *tar.Writer, zipWriter *zip.Writer) error {
	return walkArchiveEntries(ctx, source, indexPath, d, func(realPath, archivePath string, info fs.FileInfo) error {
		if !info.IsDir() {
			return addSingleFile(ctx, realPath, archivePath, zipWriter, tarWriter)
		}
		if tarWriter != nil {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = archivePath + "/"
			return tarWriter.WriteHeader(header)
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = archivePath + "/"
		header.Method = zip.Store
		_, err = zipWriter.CreateHeader(header)
		return err
	})
}

// addSingleFile writes one file into the given zip or tar writer.
func addSingleFile(ctx context.Context, realPath, archivePath string, zipWriter *zip.Writer, tarWriter *tar.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(realPath)
	if err != nil {
		if strings.Contains(err.Error(), "is a directory") {
			return nil
		}
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	if tarWriter != nil {
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(archivePath)
		if err = tarWriter.WriteHeader(header); err != nil {
			return err
		}
		_, err = io.Copy(tarWriter, archiveReader{ctx: ctx, reader: file})
		return err
	}

	if zipWriter != nil {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = archivePath
		writer, err := zipWriter.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = io.Copy(writer, archiveReader{ctx: ctx, reader: file})
		return err
	}

	return nil
}

type archiveReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r archiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// createZip writes a ZIP archive into w containing the given paths; access rules apply.
func createZip(ctx context.Context, d *Context, source string, w io.Writer, filenames ...string) error {
	zipWriter := zip.NewWriter(w)

	for _, filepath := range filenames {
		err := addFile(ctx, source, filepath, d, nil, zipWriter)
		if err != nil {
			return fmt.Errorf("add %s to ZIP: %w", filepath, err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize ZIP archive: %w", err)
	}
	return nil
}

// createTarGz writes a tar.gz archive into w containing the given paths; access rules apply.
func createTarGz(ctx context.Context, d *Context, source string, w io.Writer, filenames ...string) error {
	gzWriter := gzip.NewWriter(w)
	tarWriter := tar.NewWriter(gzWriter)

	for _, filepath := range filenames {
		err := addFile(ctx, source, filepath, d, tarWriter, nil)
		if err != nil {
			return fmt.Errorf("add %s to TAR.GZ: %w", filepath, err)
		}
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize TAR archive: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize GZIP compression: %w", err)
	}
	return nil
}

// createTarGzWithLevel writes a tar.gz archive into w with the given gzip compression level (0=default, 1-9).
func createTarGzWithLevel(ctx context.Context, d *Context, source string, w io.Writer, level int, filenames ...string) error {
	var gzWriter *gzip.Writer
	if level >= 1 && level <= 9 {
		var err error
		gzWriter, err = gzip.NewWriterLevel(w, level)
		if err != nil {
			return err
		}
	} else {
		gzWriter = gzip.NewWriter(w)
	}
	defer gzWriter.Close()
	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	for _, filepath := range filenames {
		err := addFile(ctx, source, filepath, d, tarWriter, nil)
		if err != nil {
			return fmt.Errorf("add %s to TAR.GZ: %w", filepath, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize TAR archive: %w", err)
	}
	return nil
}

// BuildAndStreamArchive writes each archive entry to the client as it is read.
func BuildAndStreamArchive(w http.ResponseWriter, r *http.Request, d *Context, source string, fileList []string) (int, error) {
	var extension string
	switch r.URL.Query().Get("algo") {
	case "zip", "true", "":
		extension = ".zip"
	case "tar.gz":
		extension = ".tar.gz"
	default:
		return http.StatusBadRequest, errors.New("archive format not implemented")
	}

	if r.URL.Query().Has("archiveToken") {
		return http.StatusGone, errors.New("archive download session expired")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return http.StatusMethodNotAllowed, nil
	}

	var download *downloadTransfer
	transferStatus := "error"
	if id := r.URL.Query().Get("archiveDownload"); r.Method == http.MethodGet && id != "" && !r.URL.Query().Has("downloadId") {
		var finish func(string)
		download, r, finish = bindDownloadTransfer(w, r, d, id)
		if download == nil {
			return http.StatusGone, errors.New("archive download expired or already started")
		}
		defer func() { finish(transferStatus) }()
	}

	idx := indexing.GetIndex(source)
	if idx == nil {
		return http.StatusInternalServerError, fmt.Errorf("source %s is not available", source)
	}
	realPath, _, err := idx.GetRealPath(fileList[0])
	if err != nil {
		return realPathErrStatus(err), err
	}

	limits, err := state.GetArchiveLimits()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if status, err := checkArchiveInputSize(r.Context(), source, fileList, d, limits.DownloadGiB); err != nil {
		return status, err
	}

	SetContentDisposition(w, r, archiveAttachmentStem(fileList, realPath)+extension, false)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		return prepareArchiveDownload(w, r, d)
	}

	stream := &archiveStreamWriter{writer: w, download: download}
	var writer io.Writer = stream
	if d.Share.MaxBandwidth > 0 {
		limit := rate.Limit(d.Share.MaxBandwidth * 1024)
		burst := d.Share.MaxBandwidth * 1024
		writer = newThrottledWriter(stream, limit, burst, r.Context())
	}
	if extension == ".zip" {
		err = createZip(r.Context(), d, source, writer, fileList...)
	} else {
		err = createTarGz(r.Context(), d, source, writer, fileList...)
	}
	if err == nil && download != nil {
		err = http.NewResponseController(w).Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	if err != nil {
		if stream.started {
			if isClientDisconnect(r, err) {
				transferStatus = "cancelled"
			} else {
				logger.Errorf("archive download interrupted: %v", err)
			}
			// Abort the response so a partial archive is not reported as a complete download.
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Content-Disposition")
		return ErrToStatus(err), err
	}
	transferStatus = "completed"
	return http.StatusOK, nil
}

type archiveStreamWriter struct {
	writer   http.ResponseWriter
	download *downloadTransfer
	started  bool
}

func (w *archiveStreamWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if w.download != nil {
		w.download.bytes.Add(int64(n))
	}
	if !w.started {
		w.started = true
		if err == nil && w.download != nil {
			w.download.transferring()
		}
		if err == nil {
			if flusher, ok := w.writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
	return n, err
}

// archiveCreateRequest is the body for POST /resources/archive (server-side create).
type archiveCreateRequest struct {
	// Source name where the paths to archive live (required). Example: "default"
	FromSource string `json:"fromSource"`
	// Source name where the archive file will be written (optional; default: fromSource). Example: "backups"
	ToSource string `json:"toSource"`
	// Paths of files or directories to add; directories are walked; access-denied entries skipped (required). Example: ["/docs/file.txt", "/photos"]
	Paths []string `json:"paths"`
	// Full path where the archive will be created; use .zip or .tar.gz extension (required). Example: "/backups/my-archive.zip"
	Destination string `json:"destination"`
	// Archive format: "zip" or "tar.gz" (optional; inferred from destination if omitted). Example: "zip"
	Format string `json:"format"`
	// Gzip compression level for tar.gz only, 0-9; 0 = default; ignored for zip (optional). Example: 6
	Compression int `json:"compression"`
	// If true, delete the source files/directories after successful archive creation (optional; requires delete permission). Example: true
	DeleteAfter bool `json:"deleteAfter"`
}

// unarchiveRequest is the body for POST /resources/unarchive (server-side extract).
type unarchiveRequest struct {
	// Source name where the archive file lives (required). Example: "default"
	FromSource string `json:"fromSource"`
	// Source name where contents will be extracted (optional; default: fromSource). Example: "restored"
	ToSource string `json:"toSource"`
	// Path to the archive file on fromSource; .zip, .tar.gz, or .tgz (required). Example: "/downloads/data.zip"
	Path string `json:"path"`
	// Directory path on toSource to extract into (required). Example: "/projects/imported"
	Destination string `json:"destination"`
	// If true, delete the archive file after successful extraction (optional; default: false). Example: true
	DeleteAfter bool `json:"deleteAfter"`
	// Fallback ZIP filename encoding; explicit Unicode metadata takes precedence.
	FilenameEncoding string `json:"filenameEncoding,omitempty"`
	// Inspect ZIP names without writing or deleting files.
	Preview bool `json:"preview,omitempty"`
}

// normalizeArchiveEntryName turns a raw zip/tar name into a safe relative path (slash-separated).
// Many Windows zips use backslashes; some start with a leading backslash, which would make
// filepath.Join drop destDir and write under the drive root. Backslashes are not path separators
// on Unix, so we always normalize '\' -> '/' and trim leading '/' before path.Clean.
func normalizeArchiveEntryName(name string) (string, error) {
	s := strings.TrimSpace(name)
	if s == "" {
		return "", errors.New("empty path")
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if strings.HasPrefix(s, "//") {
		return "", errors.New("UNC or invalid path")
	}
	s = strings.TrimLeft(s, "/")
	// Reject e.g. "C:/path" in archive
	if len(s) >= 3 && s[1] == ':' && s[2] == '/' {
		if 'A' <= s[0] && s[0] <= 'Z' || 'a' <= s[0] && s[0] <= 'z' {
			return "", errors.New("absolute path in archive")
		}
	}
	if s == ".." {
		return "", errors.New("path traversal")
	}
	if strings.HasPrefix(s, "../") {
		return "", errors.New("path traversal")
	}
	cleaned := path.Clean(s)
	if cleaned == "" || cleaned == "." {
		return "", errors.New("empty path after clean")
	}
	if cleaned == ".." {
		return "", errors.New("path traversal after clean")
	}
	if strings.HasPrefix(cleaned, "../") {
		return "", errors.New("path traversal after clean")
	}
	return cleaned, nil
}

// safeExtractPath ensures name does not escape destDir (no ".." or drive-root / UNC tricks).
func safeExtractPath(destDir, name string) (string, error) {
	relSlash, err := normalizeArchiveEntryName(name)
	if err != nil {
		return "", fmt.Errorf("invalid entry path: %q: %w", name, err)
	}
	// Convert to platform separators so filepath.Join and os.Mkdir work correctly
	abs := filepath.Join(destDir, filepath.FromSlash(relSlash))
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	entryAbs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	// Case-sensitive/prefix issues on Windows: use Rel instead of HasPrefix on absolute paths
	rel, err := filepath.Rel(destAbs, entryAbs)
	if err != nil {
		return "", fmt.Errorf("invalid entry path: %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid entry path: %q", name)
	}
	if _, err = resolvedWritePathUnderDest(destDir, entryAbs); err != nil {
		logger.Debugf("unarchive: entry %q path resolution: %v", name, err)
		return "", fmt.Errorf("invalid entry path: %q", name)
	}
	return abs, nil
}

// resolvedWritePathUnderDest resolves symlink components on entryAbs and ensures the
// effective write target stays under the resolved destination root.
func resolvedWritePathUnderDest(destDir, entryAbs string) (string, error) {
	destResolved, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		logger.Debugf("unarchive: eval destination symlinks: %v", err)
		return "", errEntryPathInvalid
	}
	check := entryAbs
	if resolved, rerr := filepath.EvalSymlinks(check); rerr == nil {
		check = resolved
	} else if errors.Is(rerr, os.ErrNotExist) {
		if li, lerr := os.Lstat(entryAbs); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
			return "", errEntryPathInvalid
		}
		parent := filepath.Dir(entryAbs)
		leaf := filepath.Base(entryAbs)
		for {
			resolvedParent, perr := filepath.EvalSymlinks(parent)
			if perr == nil {
				check = filepath.Join(resolvedParent, leaf)
				break
			}
			if !errors.Is(perr, os.ErrNotExist) {
				logger.Debugf("unarchive: eval entry symlinks: %v", perr)
				return "", errEntryPathInvalid
			}
			next := filepath.Dir(parent)
			if next == parent {
				logger.Debugf("unarchive: no existing ancestor for entry under %s", destDir)
				return "", errEntryPathInvalid
			}
			leaf = filepath.Join(filepath.Base(parent), leaf)
			parent = next
		}
	} else {
		logger.Debugf("unarchive: eval entry symlinks: %v", rerr)
		return "", errEntryPathInvalid
	}
	relCheck, err := filepath.Rel(destResolved, check)
	if err != nil {
		logger.Debugf("unarchive: relative path under destination: %v", err)
		return "", errEntryPathInvalid
	}
	if relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", errEntryPathEscape
	}
	return check, nil
}

// applyArchivedTimesAndPerm sets permission bits and modification time from an archive entry using
func applyArchivedTimesAndPerm(path string, mode fs.FileMode, modTime time.Time) error {
	switch {
	case mode.Type() == fs.ModeSymlink:
		if !modTime.IsZero() {
			_ = os.Chtimes(path, modTime, modTime)
		}
		return nil
	case mode.Type() == fs.ModeDir:
		if p := mode.Perm(); p != 0 {
			_ = os.Chmod(path, p)
		}
		if modTime.IsZero() {
			return nil
		}
		return os.Chtimes(path, modTime, modTime)
	case mode.IsRegular():
		if p := mode.Perm(); p != 0 {
			_ = os.Chmod(path, p)
		}
		if modTime.IsZero() {
			return nil
		}
		return os.Chtimes(path, modTime, modTime)
	default:
		if !modTime.IsZero() {
			_ = os.Chtimes(path, modTime, modTime)
		}
		return nil
	}
}

// Bounds symlink target resolution during archive extraction (GHSA-jg32 / #3384).
const (
	maxSymlinkResolveDepth      = 40
	maxSymlinkResolveExpansions = 255
)

// symlinkTargetStaysUnderDest rejects absolute targets and symlink text that would resolve
// outside destRoot after following symlinks already created under the destination tree.
// Lexical filepath.Clean alone is insufficient: a chained link can pass a prefix check yet
// resolve outside dest when an intermediate path component is a symlink.
func symlinkTargetStaysUnderDest(destRoot, destPath, linkname string) error {
	linkname = strings.TrimSpace(linkname)
	if linkname == "" {
		return fmt.Errorf("empty symlink target")
	}
	if filepath.IsAbs(linkname) {
		return fmt.Errorf("symlink has absolute target")
	}
	if symlinkTargetHasDotDot(linkname) {
		return fmt.Errorf("symlink target escapes destination directory")
	}
	destResolved, err := filepath.EvalSymlinks(destRoot)
	if err != nil {
		return err
	}
	linkDir, err := resolvedSymlinkParentDir(destPath)
	if err != nil {
		return fmt.Errorf("symlink target escapes destination directory")
	}
	expansionsLeft := maxSymlinkResolveExpansions
	resolved, err := resolveSymlinkTargetAt(linkDir, linkname, 0, &expansionsLeft)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(destResolved), filepath.Clean(resolved))
	if err != nil {
		return fmt.Errorf("symlink target escapes destination directory")
	}
	sep := string(filepath.Separator)
	if rel == ".." || strings.HasPrefix(rel, ".."+sep) {
		return fmt.Errorf("symlink target escapes destination directory")
	}
	return nil
}

// symlinkTargetHasDotDot reports whether linkname contains a ".." path segment before cleaning.
func symlinkTargetHasDotDot(linkname string) bool {
	linkname = filepath.ToSlash(strings.TrimSpace(linkname))
	for _, seg := range strings.Split(linkname, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// resolvedSymlinkParentDir returns the real directory that will host the new symlink at destPath.
func resolvedSymlinkParentDir(destPath string) (string, error) {
	linkPath, err := resolvedLinkPath(destPath)
	if err != nil {
		return "", err
	}
	return filepath.Dir(linkPath), nil
}

func errSymlinkTargetEscape() error {
	return fmt.Errorf("symlink target escapes destination directory")
}

func consumeSymlinkExpansion(expansionsLeft *int) error {
	if expansionsLeft == nil {
		return nil
	}
	if *expansionsLeft <= 0 {
		return errSymlinkTargetEscape()
	}
	*expansionsLeft--
	return nil
}

// resolveSymlinkTargetAt walks linkname relative to linkDir, following existing symlinks at
// each component without collapsing ".." lexically first (closes chained-link extraction bypass).
// expansionsLeft is decremented on every symlink expansion and shared across recursion.
func resolveSymlinkTargetAt(linkDir, linkname string, depth int, expansionsLeft *int) (string, error) {
	if depth > maxSymlinkResolveDepth {
		return "", errSymlinkTargetEscape()
	}
	if filepath.IsAbs(linkname) {
		return resolveSymlinkTargetAbsolute(linkname, depth, expansionsLeft)
	}
	linkDir = filepath.Clean(linkDir)
	cur := linkDir
	for _, part := range strings.Split(filepath.ToSlash(linkname), "/") {
		if part == "" || part == "." {
			continue
		}
		switch part {
		case "..":
			// Popping ".." off a path that does not exist yet is unsafe: a later archive
			// entry can create that component as a symlink, which changes where ".." lands
			// (e.g. dest/hop -> "z-pivot/..", then "z-pivot -> ." escapes dest). Only an existing
			// directory is safe to pop: a regular file can later be replaced by a symlink too.
			// Ordinary dangling links without ".." are still accepted below.
			info, err := os.Lstat(cur)
			if err != nil || !info.IsDir() {
				return "", errSymlinkTargetEscape()
			}
			cur = filepath.Dir(cur)
		default:
			next := filepath.Join(cur, part)
			info, err := os.Lstat(next)
			if err != nil {
				if os.IsNotExist(err) {
					cur = next
					continue
				}
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if err := consumeSymlinkExpansion(expansionsLeft); err != nil {
					return "", err
				}
				target, err := os.Readlink(next)
				if err != nil {
					return "", err
				}
				var nested string
				if filepath.IsAbs(target) {
					nested, err = resolveSymlinkTargetAbsolute(target, depth+1, expansionsLeft)
				} else {
					nested, err = resolveSymlinkTargetAt(filepath.Dir(next), target, depth+1, expansionsLeft)
				}
				if err != nil {
					return "", err
				}
				cur = nested
				continue
			}
			cur = next
		}
	}
	return filepath.Clean(cur), nil
}

// resolveSymlinkTargetAbsolute walks an absolute path from the volume root so every path
// component is resolved, including ancestor symlinks (a single Lstat on the full path would skip them).
// The path must not be filepath.Clean'd before walking or ".." segments collapse before symlinks are seen.
func resolveSymlinkTargetAbsolute(path string, depth int, expansionsLeft *int) (string, error) {
	if depth > maxSymlinkResolveDepth {
		return "", errSymlinkTargetEscape()
	}
	path = filepath.FromSlash(filepath.ToSlash(path))
	root := filepath.VolumeName(path) + string(filepath.Separator)
	if !strings.HasPrefix(path, root) {
		return "", errSymlinkTargetEscape()
	}
	rel := path[len(root):]
	return resolveSymlinkTargetAt(root, rel, depth, expansionsLeft)
}

// extractArchivedDir creates a directory from an archive entry and reapplies mode and mtime.
func extractArchivedDir(destPath string, mode fs.FileMode, modTime time.Time) error {
	perm := mode.Perm()
	if perm == 0 {
		perm = fileutils.EffectiveDirPerm()
	}
	if err := os.MkdirAll(destPath, perm); err != nil {
		logger.Debugf("unarchive: mkdir: %v", err)
		return fmt.Errorf("failed to create directory from archive entry")
	}
	return applyArchivedTimesAndPerm(destPath, mode, modTime)
}

// removeSymlinkDestination removes destPath when it is a symlink so a regular-file
// write creates the entry at destPath instead of following the link.
func removeSymlinkDestination(destPath string) error {
	info, err := os.Lstat(destPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	if err := os.Remove(destPath); err != nil {
		logger.Debugf("unarchive: remove symlink destination: %v", err)
		return fmt.Errorf("failed to write file from archive entry")
	}
	return nil
}

// extractArchivedRegularFile writes one regular file from src (closed by this function) and reapplies mode and mtime.
func extractArchivedRegularFile(destPath string, mode fs.FileMode, modTime time.Time, src io.ReadCloser) error {
	defer func() { _ = src.Close() }()
	parent := filepath.Dir(destPath)
	if err := os.MkdirAll(parent, fileutils.EffectiveDirPerm()); err != nil {
		logger.Debugf("unarchive: mkdir parent: %v", err)
		return fmt.Errorf("failed to create directory from archive entry")
	}
	if err := removeSymlinkDestination(destPath); err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = fileutils.EffectiveFilePerm()
	}
	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		logger.Debugf("unarchive: create file: %v", err)
		return fmt.Errorf("failed to write file from archive entry")
	}
	_, copyErr := io.Copy(out, src)
	cerr := out.Close()
	if copyErr != nil {
		logger.Debugf("unarchive: write file: %v", copyErr)
		return fmt.Errorf("failed to write file from archive entry")
	}
	if cerr != nil {
		logger.Debugf("unarchive: close file: %v", cerr)
		return fmt.Errorf("failed to write file from archive entry")
	}
	return applyArchivedTimesAndPerm(destPath, mode, modTime)
}

// extractArchivedSymlink creates a symlink after validating the target stays under destRoot.
func extractArchivedSymlink(destRoot, destPath, target string, modTime time.Time) error {
	target = strings.TrimSpace(target)
	symParent := filepath.Dir(destPath)
	if err := os.MkdirAll(symParent, fileutils.EffectiveDirPerm()); err != nil {
		logger.Debugf("unarchive: mkdir symlink parent: %v", err)
		return fmt.Errorf("failed to create symlink from archive entry")
	}
	if err := symlinkTargetStaysUnderDest(destRoot, destPath, target); err != nil {
		return err
	}
	// Never replace a directory, or a symlink that resolves to one, with a new symlink. os.Remove
	// would delete an empty directory, and a pre-existing ".." link through it could then escape.
	// os.Stat follows symlinks, so an alias such as alias -> sub is covered too. A missing
	// destination passes through.
	if info, err := os.Stat(destPath); err == nil && info.IsDir() {
		return fmt.Errorf("symlink from archive entry would replace a directory")
	}
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		logger.Debugf("unarchive: remove symlink: %v", err)
		return fmt.Errorf("failed to create symlink from archive entry")
	}
	if err := os.Symlink(target, destPath); err != nil {
		logger.Debugf("unarchive: symlink: %v", err)
		return fmt.Errorf("failed to create symlink from archive entry")
	}
	indexing.ClearRealPathCaches()
	return applyArchivedTimesAndPerm(destPath, fs.ModeSymlink, modTime)
}

// extractArchivedHardLink writes a copy of a regular file extracted earlier from the same
// archive, which is what a tar hard link entry stands for.
func extractArchivedHardLink(destRoot, destPath string, h *tar.Header, extracted map[string]os.FileInfo) error {
	targetPath, err := safeExtractPath(destRoot, h.Linkname)
	if err != nil {
		return err
	}
	notExtracted := fmt.Errorf("hard link %q points to %q, which is not a file earlier in the archive", h.Name, h.Linkname)
	written, ok := extracted[targetPath]
	if !ok {
		return notExtracted
	}
	src, err := os.Open(targetPath)
	if err != nil {
		logger.Debugf("unarchive: open hard link target: %v", err)
		return fmt.Errorf("failed to write file from archive entry")
	}
	// A later entry may have replaced the target, e.g. with a symlink to another file.
	info, err := src.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, written) {
		_ = src.Close()
		return notExtracted
	}
	// The entry may name its own target, directly (GNU tar does this for a file archived
	// twice). Writing it would truncate the file it copies from. Symlinks at destPath
	// are replaced by extractArchivedRegularFile instead of being left in place.
	if dest, err := os.Lstat(destPath); err == nil && dest.Mode().IsRegular() && os.SameFile(dest, info) {
		return src.Close()
	}
	return extractArchivedRegularFile(destPath, h.FileInfo().Mode(), h.ModTime, src)
}

// recordExtractedFile remembers the regular file written at destPath for later hard links.
func recordExtractedFile(extracted map[string]os.FileInfo, destPath string) {
	if info, err := os.Lstat(destPath); err == nil && info.Mode().IsRegular() {
		extracted[destPath] = info
	} else {
		delete(extracted, destPath)
	}
}

func extractZip(archivePath, destDir string) error {
	return extractZipWithEncoding(archivePath, destDir, "", nil)
}

func extractZipWithEncoding(archivePath, destDir, charset string, policy *extractAccessPolicy) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()
	names, err := archiveencoding.DecodeNames(r.File, charset)
	if err != nil {
		return err
	}
	// Decode before interpreting separators: CP932 characters can contain byte 0x5c.
	// Validate every name before creating any files, including normalized collisions.
	destinations := make(map[string]string, len(names))
	for i, name := range names {
		dest, pathErr := safeExtractPath(destDir, name)
		if pathErr != nil {
			return pathErr
		}
		if original, exists := destinations[dest]; exists && original != r.File[i].Name {
			return fmt.Errorf("ZIP filenames resolve to the same destination: %q", name)
		}
		destinations[dest] = r.File[i].Name
	}

	// Central directory order is arbitrary. Some zips (e.g. Windows driver packs) use names that
	// are similar length at different tree depths, so byte length is a poor order key. Use the
	// number of path segments in the *normalized* entry name: deeper trees first, then
	// lexicographic, so parent dirs exist before shorter entries that might be mis-tagged.
	type orderedName struct {
		f     *zip.File
		name  string
		rel   string
		depth int
	}
	ordered := make([]orderedName, 0, len(r.File))
	for i, f := range r.File {
		name := names[i]
		rel, nerr := normalizeArchiveEntryName(name)
		if nerr != nil {
			rel = strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"), "/")
		}
		depth := 0
		if rel != "" {
			depth = strings.Count(rel, "/") + 1
		}
		ordered = append(ordered, orderedName{f: f, name: name, rel: rel, depth: depth})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].depth != ordered[j].depth {
			return ordered[i].depth > ordered[j].depth
		}
		if ordered[i].rel != ordered[j].rel {
			return ordered[i].rel < ordered[j].rel
		}
		return ordered[i].f.Name < ordered[j].f.Name
	})

	for _, o := range ordered {
		destPath, pathErr := safeExtractPath(destDir, o.name)
		if pathErr != nil {
			return pathErr
		}
		fi := o.f.FileInfo()
		mode := fi.Mode()
		isDirEntry := mode.Type() == fs.ModeDir || strings.HasSuffix(o.name, "/") || strings.HasSuffix(o.name, "\\")
		isSymlink := mode.Type() == fs.ModeSymlink
		if !isDirEntry && !isSymlink && !mode.IsRegular() {
			continue
		}
		if err = preflightExtractEntry(policy, o.name, destPath, isDirEntry, isSymlink); err != nil {
			return err
		}
	}

	for _, o := range ordered {
		f := o.f
		var destPath string
		destPath, err = safeExtractPath(destDir, o.name)
		if err != nil {
			return err
		}

		fi := f.FileInfo()
		mode := fi.Mode()
		isDirEntry := mode.Type() == fs.ModeDir || strings.HasSuffix(o.name, "/") || strings.HasSuffix(o.name, "\\")
		if isDirEntry {
			if err = extractArchivedDir(destPath, mode, f.Modified); err != nil {
				return err
			}
			continue
		}

		if mode.Type() == fs.ModeSymlink {
			var rc io.ReadCloser
			rc, err = f.Open()
			if err != nil {
				return err
			}
			buf, rerr := io.ReadAll(rc)
			rc.Close()
			if rerr != nil {
				return rerr
			}
			if err = extractArchivedSymlink(destDir, destPath, string(buf), f.Modified); err != nil {
				return err
			}
			continue
		}

		if !mode.IsRegular() {
			continue
		}

		var rc io.ReadCloser
		rc, err = f.Open()
		if err != nil {
			return err
		}
		if err = extractArchivedRegularFile(destPath, mode, f.Modified, rc); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(archivePath, destDir string, policy *extractAccessPolicy) error {
	if err := preflightTarGz(archivePath, destDir, policy); err != nil {
		return err
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	// Regular files written so far, for hard links to point at.
	extracted := make(map[string]os.FileInfo)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		destPath, err := safeExtractPath(destDir, h.Name)
		if err != nil {
			return err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			mode := h.FileInfo().Mode()
			if err = extractArchivedDir(destPath, mode, h.ModTime); err != nil {
				return err
			}
		case tar.TypeReg:
			mode := h.FileInfo().Mode()
			if err = extractArchivedRegularFile(destPath, mode, h.ModTime, io.NopCloser(tr)); err != nil {
				return err
			}
			recordExtractedFile(extracted, destPath)
		case tar.TypeLink:
			if err = extractArchivedHardLink(destDir, destPath, h, extracted); err != nil {
				return err
			}
			recordExtractedFile(extracted, destPath)
		case tar.TypeSymlink:
			if err = extractArchivedSymlink(destDir, destPath, h.Linkname, h.ModTime); err != nil {
				return err
			}
		default:
			if h.Size > 0 {
				if _, err = io.CopyN(io.Discard, tr, h.Size); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func preflightTarGz(archivePath, destDir string, policy *extractAccessPolicy) error {
	if policy == nil {
		return nil
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		destPath, err := safeExtractPath(destDir, h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err = preflightExtractEntry(policy, h.Name, destPath, true, false); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeLink:
			if err = preflightExtractEntry(policy, h.Name, destPath, false, false); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err = preflightExtractEntry(policy, h.Name, destPath, false, true); err != nil {
				return err
			}
		default:
			if h.Size > 0 {
				if _, err = io.CopyN(io.Discard, tr, h.Size); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkArchiveInputSize(ctx context.Context, source string, paths []string, d *Context, limitGiB int64) (int, error) {
	if limitGiB <= 0 {
		return 0, nil
	}
	size, err := computeArchiveSize(ctx, source, paths, d)
	if err != nil {
		return ErrToStatus(err), fmt.Errorf("compute archive input size: %w", err)
	}
	if err := checkArchiveSizeLimit(size, limitGiB); err != nil {
		return http.StatusRequestEntityTooLarge, err
	}
	return 0, nil
}

func checkArchiveSizeLimit(size, limitGiB int64) error {
	if limitGiB > 0 && size > 0 && (size-1)/(1<<30) >= limitGiB {
		return fmt.Errorf("archive exceeds the configured size limit of %d GiB", limitGiB)
	}
	return nil
}

// computeArchiveSize counts only the regular files that will be put in the archive.
func computeArchiveSize(ctx context.Context, source string, paths []string, d *Context) (int64, error) {
	var size int64
	for _, indexPath := range paths {
		err := walkArchiveEntries(ctx, source, indexPath, d, func(_ string, _ string, info fs.FileInfo) error {
			if info.Mode().IsRegular() {
				if info.Size() > math.MaxInt64-size {
					size = math.MaxInt64
				} else {
					size += info.Size()
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return size, nil
}
