package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed podcast_gen.py
var embeddedPodcastScript []byte

// version is the build version. It defaults to "dev" for local builds and is
// overridden at release time via -ldflags "-X main.version=<tag>".
var version = "dev"

const mdviewerFile = ".mdviewer"

// githubRepo is the "owner/name" used by the self-update command to locate
// release assets.
const githubRepo = "roraja/markdown-go"

// mdviewerDataDir returns the path to the mdviewer data directory
// (~/.local/mdviewer), creating it if it doesn't exist.
func mdviewerDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".local", "mdviewer")
	os.MkdirAll(dir, 0755)
	return dir
}

// ensureEmbeddedPodcastScript extracts the embedded podcast_gen.py to
// ~/.local/mdviewer/podcast_gen.py so the binary is self-contained. Returns
// the path to the extracted script.
func ensureEmbeddedPodcastScript() string {
	dir := mdviewerDataDir()
	if dir == "" {
		return ""
	}
	dest := filepath.Join(dir, "podcast_gen.py")

	// Always overwrite — the embedded version matches this binary's release.
	if err := os.WriteFile(dest, embeddedPodcastScript, 0755); err != nil {
		log.Printf("Warning: failed to extract embedded podcast_gen.py: %v", err)
		return ""
	}
	return dest
}

var validTags = map[string]bool{
	"DONE":        true,
	"IN-PROGRESS": true,
	"NEXT":        true,
	"IMPORTANT":   true,
	"REVISIT":     true,
	"ARCHIVE":     true,
}

type app struct {
	root string
	tpl  *template.Template

	// Podcast generation state
	podcastMu   sync.Mutex
	podcastJobs map[string]*podcastJob // keyed by relative md path
}

type podcastJob struct {
	Status   string   `json:"status"`   // "generating", "done", "error"
	Progress int      `json:"progress"` // 0-100
	Error    string   `json:"error,omitempty"`
	Output   string   `json:"output,omitempty"` // relative path to mp3
	Lines    []string `json:"lines,omitempty"`  // live log lines
}

type pageData struct {
	Root        string
	InitialFile string
}

type mdviewerData struct {
	Tags     map[string][]string `json:"tags"`
	Opened   map[string]bool     `json:"opened"`
	LogViews []logViewEntry      `json:"logViews,omitempty"`
}

// logViewEntry is a saved, Notion-like log view (filters + column config)
// stored in a .mdviewer file. It applies to all log files in that directory
// and its subdirectories.
type logViewEntry struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
}

// mdviewerDataLegacy handles the old single-tag format for migration.
type mdviewerDataLegacy struct {
	Tags map[string]string `json:"tags"`
}

func readMdviewerFile(dirPath string) (mdviewerData, error) {
	data := mdviewerData{Tags: make(map[string][]string), Opened: make(map[string]bool)}
	fp := filepath.Join(dirPath, mdviewerFile)
	content, err := os.ReadFile(fp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return data, nil
		}
		return data, err
	}
	if err := json.Unmarshal(content, &data); err != nil {
		// Try legacy single-tag format
		var legacy mdviewerDataLegacy
		if err2 := json.Unmarshal(content, &legacy); err2 == nil && legacy.Tags != nil {
			data.Tags = make(map[string][]string)
			for k, v := range legacy.Tags {
				if v != "" {
					data.Tags[k] = []string{v}
				}
			}
			return data, nil
		}
		return mdviewerData{Tags: make(map[string][]string), Opened: make(map[string]bool)}, nil
	}
	if data.Tags == nil {
		data.Tags = make(map[string][]string)
	}
	if data.Opened == nil {
		data.Opened = make(map[string]bool)
	}
	return data, nil
}

func writeMdviewerFile(dirPath string, data mdviewerData) error {
	fp := filepath.Join(dirPath, mdviewerFile)
	if len(data.Tags) == 0 && len(data.Opened) == 0 && len(data.LogViews) == 0 {
		_ = os.Remove(fp)
		return nil
	}
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(fp, content, 0644)
}

type allTagsResult struct {
	Tags   map[string][]string `json:"tags"`
	Opened map[string]bool     `json:"opened"`
}

// collectAllTags walks the root and reads all .mdviewer files, returning tags and opened state per file.
func collectAllTags(root string) (allTagsResult, error) {
	result := allTagsResult{
		Tags:   make(map[string][]string),
		Opened: make(map[string]bool),
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || d.Name() != mdviewerFile {
			return nil
		}
		dirPath := filepath.Dir(path)
		data, err := readMdviewerFile(dirPath)
		if err != nil {
			return nil
		}
		relDir, err := filepath.Rel(root, dirPath)
		if err != nil {
			return nil
		}
		for fileName, tags := range data.Tags {
			var relFile string
			if relDir == "." {
				relFile = fileName
			} else {
				relFile = filepath.ToSlash(filepath.Join(relDir, fileName))
			}
			result.Tags[relFile] = tags
		}
		for fileName, opened := range data.Opened {
			if opened {
				var relFile string
				if relDir == "." {
					relFile = fileName
				} else {
					relFile = filepath.ToSlash(filepath.Join(relDir, fileName))
				}
				result.Opened[relFile] = true
			}
		}
		return nil
	})
	return result, err
}

func main() {
	rootFlag := flag.String("root", ".", "Root directory to scan for markdown files")
	portFlag := flag.String("port", "8080", "HTTP port to listen on")
	podcastWatchFlag := flag.String("podcast-watch", "", "Comma-separated list of directories (relative to -root) to watch for auto podcast generation")
	versionFlag := flag.Bool("version", false, "Print version and exit")
	updateFlag := flag.Bool("update", false, "Update mdviewer to the latest GitHub release and exit")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("mdviewer %s\n", version)
		return
	}

	if *updateFlag {
		if err := selfUpdate(); err != nil {
			log.Fatalf("update failed: %v", err)
		}
		return
	}

	absRoot, err := filepath.Abs(*rootFlag)
	if err != nil {
		log.Fatalf("resolve root: %v", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		log.Fatalf("stat root: %v", err)
	}
	if !info.IsDir() {
		log.Fatalf("root is not a directory: %s", absRoot)
	}

	tpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		log.Fatalf("parse template: %v", err)
	}

	a := &app{root: absRoot, tpl: tpl, podcastJobs: make(map[string]*podcastJob)}

	// Extract embedded podcast_gen.py to ~/.local/mdviewer/ so it's always available
	if p := ensureEmbeddedPodcastScript(); p != "" {
		log.Printf("Extracted podcast_gen.py to %s", p)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/files", a.handleFiles)
	mux.HandleFunc("/api/file", a.handleFile)
	mux.HandleFunc("/api/log", a.handleLogTail)
	mux.HandleFunc("/api/log/clear", a.handleLogClear)
	mux.HandleFunc("/api/log/views", a.handleLogViews)
	mux.HandleFunc("/api/log/views/save", a.handleLogViewSave)
	mux.HandleFunc("/api/log/views/delete", a.handleLogViewDelete)
	mux.HandleFunc("/api/search", a.handleSearch)
	mux.HandleFunc("/api/tags", a.handleTags)
	mux.HandleFunc("/api/tag", a.handleSetTag)
	mux.HandleFunc("/api/opened", a.handleMarkOpened)
	mux.HandleFunc("/api/archive", a.handleArchive)
	mux.HandleFunc("/api/save", a.handleSave)
	mux.HandleFunc("/api/media/", a.handleMedia)
	mux.HandleFunc("/api/podcast", a.handlePodcast)
	mux.HandleFunc("/podcasts", a.handlePodcasts)
	mux.HandleFunc("/api/podcasts", a.handlePodcastList)
	mux.HandleFunc("/api/podcasts/progress", a.handlePodcastProgress)
	mux.HandleFunc("/api/podcasts/queue", a.handlePodcastQueue)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	if *podcastWatchFlag != "" {
		entries := strings.Split(*podcastWatchFlag, ",")
		var dirs []string
		var patterns []string
		for _, e := range entries {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if strings.ContainsAny(e, "*?") {
				patterns = append(patterns, e)
			} else {
				dirs = append(dirs, e)
			}
		}
		go a.startPodcastWatcher(dirs, patterns)
	}

	addr := ":" + *portFlag
	log.Printf("Markdown viewer running on http://localhost%s (root: %s)", addr, absRoot)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// releaseAssetName returns the release asset file name for the current
// platform, matching the names produced by .github/workflows/release.yml.
func releaseAssetName() string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("mdviewer-%s-%s%s", osName, runtime.GOARCH, ext)
}

// selfUpdate downloads the latest release binary for this platform from
// GitHub and replaces the currently running executable in place.
func selfUpdate() error {
	assetName := releaseAssetName()
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)

	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mdviewer-self-update")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GitHub API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return fmt.Errorf("decode release: %w", err)
	}

	fmt.Printf("Current version: %s\n", version)
	fmt.Printf("Latest release:  %s\n", release.TagName)
	if release.TagName != "" && ("v"+version == release.TagName || version == release.TagName) {
		fmt.Println("Already up to date.")
		return nil
	}

	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("no release asset %q found in %s", assetName, release.TagName)
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	fmt.Printf("Downloading %s …\n", assetName)
	dlReq, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	dlReq.Header.Set("User-Agent", "mdviewer-self-update")
	dlResp, err := client.Do(dlReq)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %s", dlResp.Status)
	}

	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".mdviewer-update-*")
	if err != nil {
		return fmt.Errorf("create temp file (need write access to %s): %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, dlResp.Body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write binary: %w", err)
	}
	tmp.Close()
	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return err
	}

	if runtime.GOOS == "windows" {
		// A running .exe can't be overwritten on Windows; move it aside first.
		old := exePath + ".old"
		os.Remove(old)
		if err := os.Rename(exePath, old); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("replace executable: %w", err)
		}
		if err := os.Rename(tmpName, exePath); err != nil {
			os.Rename(old, exePath)
			return fmt.Errorf("install new executable: %w", err)
		}
		os.Remove(old)
	} else {
		if err := os.Rename(tmpName, exePath); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("replace executable at %s: %w", exePath, err)
		}
	}

	fmt.Printf("Updated mdviewer to %s at %s\n", release.TagName, exePath)
	return nil
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	initialFile := r.URL.Query().Get("file")
	if _, err := sanitizeRelativePath(initialFile); err != nil {
		initialFile = ""
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tpl.Execute(w, pageData{
		Root:        a.root,
		InitialFile: initialFile,
	}); err != nil {
		http.Error(w, "template render failed", http.StatusInternalServerError)
	}
}

type fileMeta struct {
	Path       string `json:"path"`
	ModifiedAt int64  `json:"modifiedAt"`
	CreatedAt  int64  `json:"createdAt"`
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	files, err := listMarkdownFiles(a.root)
	if err != nil {
		http.Error(w, "failed to list markdown files", http.StatusInternalServerError)
		return
	}

	meta := make([]fileMeta, 0, len(files))
	for _, f := range files {
		fullPath := filepath.Join(a.root, filepath.FromSlash(f))
		info, err := os.Stat(fullPath)
		m := fileMeta{Path: f}
		if err == nil {
			m.ModifiedAt = info.ModTime().UnixMilli()
			m.CreatedAt = info.ModTime().UnixMilli() // fallback; true ctime not portable
		}
		meta = append(meta, m)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Root  string     `json:"root"`
		Files []string   `json:"files"`
		Meta  []fileMeta `json:"meta"`
	}{
		Root:  a.root,
		Files: files,
		Meta:  meta,
	})
}

func (a *app) handleFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requested := r.URL.Query().Get("path")
	relPath, err := sanitizeRelativePath(requested)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isMarkdownFile(relPath) {
		http.Error(w, "only markdown files are supported", http.StatusBadRequest)
		return
	}

	fullPath, err := secureJoin(a.root, relPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{
		Path:    relPath,
		Content: string(content),
	})
}

type searchResult struct {
	Path    string `json:"path"`
	Context string `json:"context"`
}

// maxLogInitialBytes caps the initial log payload so opening a huge log file
// only loads the most recent tail rather than the entire file.
const maxLogInitialBytes = 2 << 20 // 2 MiB

// handleLogTail streams new bytes from a log file. With no offset (or a
// negative one) it returns the tail of the file (capped to maxLogInitialBytes)
// and the current size as the next offset. With an offset it returns only the
// bytes written since that offset, detecting truncation/rotation.
func (a *app) handleLogTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relPath, err := sanitizeRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isLogFile(relPath) {
		http.Error(w, "only log files are supported", http.StatusBadRequest)
		return
	}

	fullPath, err := secureJoin(a.root, relPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	f, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	size := info.Size()

	offset := int64(-1)
	hasOffset := false
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if v, perr := strconv.ParseInt(raw, 10, 64); perr == nil {
			offset = v
			hasOffset = true
		}
	}

	truncated := false
	var start int64
	switch {
	case !hasOffset || offset < 0:
		// Initial load: return the tail capped to maxLogInitialBytes.
		if size > maxLogInitialBytes {
			start = size - maxLogInitialBytes
			truncated = true // indicates content was clipped from the front
		}
	case offset > size:
		// File shrank (cleared/rotated) — re-read from the beginning.
		start = 0
		truncated = true
	default:
		start = offset
	}

	var content []byte
	if start < size {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}
		buf := make([]byte, size-start)
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}
		content = buf[:n]
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Offset    int64  `json:"offset"`
		Size      int64  `json:"size"`
		Truncated bool   `json:"truncated"`
	}{
		Path:      relPath,
		Content:   string(content),
		Offset:    size,
		Size:      size,
		Truncated: truncated,
	})
}

// handleLogClear truncates a log file to zero length.
func (a *app) handleLogClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	relPath, err := sanitizeRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isLogFile(relPath) {
		http.Error(w, "only log files are supported", http.StatusBadRequest)
		return
	}

	fullPath, err := secureJoin(a.root, relPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if err := os.Truncate(fullPath, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to clear file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// logViewResponse is a saved view returned to the client, annotated with the
// directory (relative to root) of the .mdviewer file that stores it so the
// client can later delete it from the correct place.
type logViewResponse struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	Dir    string          `json:"dir"`
}

// logViewDirs returns the chain of directories (relative to root, root-first)
// from the root down to the directory containing relPath. These are the
// directories whose .mdviewer views apply to relPath.
func logViewDirs(relPath string) []string {
	dir := filepath.ToSlash(filepath.Dir(relPath))
	dirs := []string{""}
	if dir == "." || dir == "" {
		return dirs
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		dirs = append(dirs, cur)
	}
	return dirs
}

// handleLogViews returns all saved log views applicable to a log file: those
// stored in the file's own directory and any ancestor up to the root. Views
// declared deeper in the tree override same-named ancestors.
func (a *app) handleLogViews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath, err := sanitizeRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	byName := make(map[string]logViewResponse)
	var order []string
	for _, relDir := range logViewDirs(relPath) {
		dirFull, derr := secureJoin(a.root, relDir)
		if relDir == "" {
			dirFull = a.root
			derr = nil
		}
		if derr != nil {
			continue
		}
		data, rerr := readMdviewerFile(dirFull)
		if rerr != nil {
			continue
		}
		for _, v := range data.LogViews {
			if _, exists := byName[v.Name]; !exists {
				order = append(order, v.Name)
			}
			byName[v.Name] = logViewResponse{Name: v.Name, Config: v.Config, Dir: relDir}
		}
	}

	views := make([]logViewResponse, 0, len(order))
	for _, n := range order {
		views = append(views, byName[n])
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"views": views})
}

// handleLogViewSave upserts a named view into the .mdviewer file of the log
// file's own directory, making it available to that directory and all
// subdirectories.
func (a *app) handleLogViewSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath, err := sanitizeRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	var body struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if len(body.Config) == 0 {
		body.Config = json.RawMessage("{}")
	}

	relDir := filepath.ToSlash(filepath.Dir(relPath))
	var dirFull string
	if relDir == "." || relDir == "" {
		relDir = ""
		dirFull = a.root
	} else {
		dirFull, err = secureJoin(a.root, relDir)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}

	data, err := readMdviewerFile(dirFull)
	if err != nil {
		http.Error(w, "failed to read views", http.StatusInternalServerError)
		return
	}
	updated := false
	for i := range data.LogViews {
		if data.LogViews[i].Name == body.Name {
			data.LogViews[i].Config = body.Config
			updated = true
			break
		}
	}
	if !updated {
		data.LogViews = append(data.LogViews, logViewEntry{Name: body.Name, Config: body.Config})
	}
	if err := writeMdviewerFile(dirFull, data); err != nil {
		http.Error(w, "failed to save view", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "dir": relDir})
}

// handleLogViewDelete removes a named view. It searches from the log file's
// own directory upward to the root and deletes the first match.
func (a *app) handleLogViewDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath, err := sanitizeRelativePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	dirs := logViewDirs(relPath)
	// Search deepest-first so a delete removes the most specific definition.
	for i := len(dirs) - 1; i >= 0; i-- {
		relDir := dirs[i]
		dirFull := a.root
		if relDir != "" {
			dirFull, err = secureJoin(a.root, relDir)
			if err != nil {
				continue
			}
		}
		data, rerr := readMdviewerFile(dirFull)
		if rerr != nil {
			continue
		}
		idx := -1
		for j := range data.LogViews {
			if data.LogViews[j].Name == body.Name {
				idx = j
				break
			}
		}
		if idx >= 0 {
			data.LogViews = append(data.LogViews[:idx], data.LogViews[idx+1:]...)
			if err := writeMdviewerFile(dirFull, data); err != nil {
				http.Error(w, "failed to delete view", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}
	}
	http.Error(w, "view not found", http.StatusNotFound)
}

func (a *app) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "missing query parameter 'q'", http.StatusBadRequest)
		return
	}

	results, err := searchFiles(a.root, query)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Query   string         `json:"query"`
		Results []searchResult `json:"results"`
	}{
		Query:   query,
		Results: results,
	})
}

func (a *app) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := collectAllTags(a.root)
	if err != nil {
		http.Error(w, "failed to read tags", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(result)
}

func (a *app) handleSetTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path   string `json:"path"`
		Tag    string `json:"tag"`
		Action string `json:"action"` // "add", "remove", or "clear"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	relPath, err := sanitizeRelativePath(req.Path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isMarkdownFile(relPath) {
		http.Error(w, "only markdown files can be tagged", http.StatusBadRequest)
		return
	}
	if req.Action == "" {
		req.Action = "add"
	}
	if req.Action != "add" && req.Action != "remove" && req.Action != "clear" {
		http.Error(w, "invalid action", http.StatusBadRequest)
		return
	}
	if req.Action != "clear" && req.Tag != "" && !validTags[req.Tag] {
		http.Error(w, "invalid tag", http.StatusBadRequest)
		return
	}

	dirRel := filepath.Dir(relPath)
	fileName := filepath.Base(relPath)
	dirAbs, err := secureJoin(a.root, dirRel)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	data, err := readMdviewerFile(dirAbs)
	if err != nil {
		http.Error(w, "failed to read tags", http.StatusInternalServerError)
		return
	}

	switch req.Action {
	case "clear":
		delete(data.Tags, fileName)
	case "remove":
		if tags, ok := data.Tags[fileName]; ok {
			filtered := make([]string, 0, len(tags))
			for _, t := range tags {
				if t != req.Tag {
					filtered = append(filtered, t)
				}
			}
			if len(filtered) == 0 {
				delete(data.Tags, fileName)
			} else {
				data.Tags[fileName] = filtered
			}
		}
	case "add":
		if req.Tag != "" {
			existing := data.Tags[fileName]
			found := false
			for _, t := range existing {
				if t == req.Tag {
					found = true
					break
				}
			}
			if !found {
				data.Tags[fileName] = append(existing, req.Tag)
			}
		}
	}

	if err := writeMdviewerFile(dirAbs, data); err != nil {
		http.Error(w, "failed to write tags", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
	}{OK: true})
}

func (a *app) handleMarkOpened(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	relPath, err := sanitizeRelativePath(req.Path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isMarkdownFile(relPath) {
		http.Error(w, "only markdown files supported", http.StatusBadRequest)
		return
	}

	dirRel := filepath.Dir(relPath)
	fileName := filepath.Base(relPath)
	dirAbs, err := secureJoin(a.root, dirRel)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	data, err := readMdviewerFile(dirAbs)
	if err != nil {
		http.Error(w, "failed to read data", http.StatusInternalServerError)
		return
	}

	data.Opened[fileName] = true

	if err := writeMdviewerFile(dirAbs, data); err != nil {
		http.Error(w, "failed to write data", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
	}{OK: true})
}

func (a *app) handleMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Strip /api/media/ prefix to get relative path
	relPath := strings.TrimPrefix(r.URL.Path, "/api/media/")
	if relPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}

	// Clean and validate path to prevent directory traversal
	cleaned := filepath.Clean(filepath.FromSlash(relPath))
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	fullPath := filepath.Join(a.root, cleaned)

	// Verify the resolved path is within root
	absRoot, _ := filepath.Abs(a.root)
	absPath, _ := filepath.Abs(fullPath)
	if !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) && absPath != absRoot {
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	http.ServeFile(w, r, fullPath)
}

func (a *app) handlePodcast(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}

	relPath, err := sanitizeRelativePath(filePath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch r.Method {
	case http.MethodGet:
		// Check status or serve existing podcast
		a.podcastMu.Lock()
		job, exists := a.podcastJobs[relPath]
		a.podcastMu.Unlock()

		if exists {
			_ = json.NewEncoder(w).Encode(job)
			return
		}

		// Check if mp3 already exists on disk
		mp3Path := a.podcastMP3Path(relPath)
		if _, err := os.Stat(mp3Path); err == nil {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "done",
				"output": a.podcastMP3RelPath(relPath),
			})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "none"})

	case http.MethodPost:
		// Start generation
		a.podcastMu.Lock()
		if job, exists := a.podcastJobs[relPath]; exists && job.Status == "generating" {
			a.podcastMu.Unlock()
			_ = json.NewEncoder(w).Encode(job)
			return
		}

		job := &podcastJob{Status: "generating", Progress: 0}
		a.podcastJobs[relPath] = job
		a.podcastMu.Unlock()

		go a.generatePodcast(relPath, job)

		_ = json.NewEncoder(w).Encode(job)

	case http.MethodDelete:
		// Delete cached podcast
		mp3Path := a.podcastMP3Path(relPath)
		scriptPath := a.podcastScriptPath(relPath)
		_ = os.Remove(mp3Path)
		_ = os.Remove(scriptPath)
		a.podcastMu.Lock()
		delete(a.podcastJobs, relPath)
		a.podcastMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) podcastMP3Path(relPath string) string {
	ext := filepath.Ext(relPath)
	base := relPath[:len(relPath)-len(ext)]
	return filepath.Join(a.root, base+".podcast.mp3")
}

func (a *app) podcastScriptPath(relPath string) string {
	ext := filepath.Ext(relPath)
	base := relPath[:len(relPath)-len(ext)]
	return filepath.Join(a.root, base+".podcast-script.txt")
}

func (a *app) podcastMP3RelPath(relPath string) string {
	ext := filepath.Ext(relPath)
	base := relPath[:len(relPath)-len(ext)]
	return base + ".podcast.mp3"
}

func (a *app) generatePodcast(relPath string, job *podcastJob) {
	fullPath := filepath.Join(a.root, relPath)
	mp3Path := a.podcastMP3Path(relPath)

	// Ensure log directory exists
	logDir := filepath.Join(mdviewerDataDir(), "logs")
	os.MkdirAll(logDir, 0755)
	logFile := filepath.Join(logDir, "podcast.log")

	appendLog := func(msg string) {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintf(f, "[%s] [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), relPath, msg)
			f.Close()
		}
	}

	appendLog("Starting podcast generation")

	// Find podcast_gen.py — prefer the embedded copy extracted at startup,
	// then check next to the binary, standard install path, and dev source.
	home, _ := os.UserHomeDir()
	scriptLocations := []string{
		filepath.Join(mdviewerDataDir(), "podcast_gen.py"),
		filepath.Join(filepath.Dir(os.Args[0]), "podcast_gen.py"),
		"/usr/local/share/mdviewer/podcast_gen.py",
		filepath.Join(home, "src", "markdown-go", "podcast_gen.py"),
	}

	var scriptPath string
	for _, loc := range scriptLocations {
		if _, err := os.Stat(loc); err == nil {
			scriptPath = loc
			break
		}
	}

	if scriptPath == "" {
		appendLog("ERROR: podcast_gen.py not found in any search path")
		a.podcastMu.Lock()
		job.Status = "error"
		job.Error = "podcast_gen.py not found"
		a.podcastMu.Unlock()
		return
	}

	appendLog(fmt.Sprintf("Using script: %s", scriptPath))

	cmd := exec.Command("python3", scriptPath, fullPath, "-o", mp3Path)
	// Pass through all env vars — podcast_gen.py auto-detects provider from
	// PODCAST_API_URL, PODCAST_API_TOKEN, OPENCLAW_TOKEN, OPENAI_API_KEY, ANTHROPIC_API_KEY
	cmd.Env = os.Environ()

	// Log env vars relevant to podcast generation (redacted)
	for _, key := range []string{"PODCAST_API_URL", "PODCAST_API_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		val := os.Getenv(key)
		if val != "" {
			appendLog(fmt.Sprintf("Env %s=<set> (len=%d)", key, len(val)))
		} else {
			appendLog(fmt.Sprintf("Env %s=<not set>", key))
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		appendLog(fmt.Sprintf("ERROR: stdout pipe: %v", err))
		a.podcastMu.Lock()
		job.Status = "error"
		job.Error = err.Error()
		a.podcastMu.Unlock()
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		appendLog(fmt.Sprintf("ERROR: cmd start: %v", err))
		a.podcastMu.Lock()
		job.Status = "error"
		job.Error = err.Error()
		a.podcastMu.Unlock()
		return
	}

	appendLog(fmt.Sprintf("Process started PID=%d", cmd.Process.Pid))

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		appendLog(fmt.Sprintf("OUT: %s", line))
		a.podcastMu.Lock()
		job.Lines = append(job.Lines, line)
		// Parse progress from [tts] NN% lines
		if strings.HasPrefix(line, "[tts]") {
			var pct int
			if _, err := fmt.Sscanf(line, "[tts] %d%%", &pct); err == nil {
				job.Progress = pct
			}
		}
		a.podcastMu.Unlock()
	}

	if err := cmd.Wait(); err != nil {
		appendLog(fmt.Sprintf("ERROR: process exited: %v", err))
		a.podcastMu.Lock()
		job.Status = "error"
		job.Error = fmt.Sprintf("podcast_gen.py failed: %v", err)
		a.podcastMu.Unlock()
		return
	}

	appendLog(fmt.Sprintf("SUCCESS: generated %s", mp3Path))

	a.podcastMu.Lock()
	job.Status = "done"
	job.Progress = 100
	job.Output = a.podcastMP3RelPath(relPath)
	a.podcastMu.Unlock()

	log.Printf("Podcast generated: %s", mp3Path)
}

// --- Podcast Auto-Watch ---

func (a *app) startPodcastWatcher(dirs []string, patterns []string) {
	stateFile := filepath.Join(mdviewerDataDir(), "podcast-watch-state.json")
	os.MkdirAll(filepath.Dir(stateFile), 0755)

	// State: map of relPath -> mod time (unix)
	known := make(map[string]int64)

	// Load existing state
	if data, err := os.ReadFile(stateFile); err == nil {
		json.Unmarshal(data, &known)
	}

	saveState := func() {
		data, _ := json.Marshal(known)
		os.WriteFile(stateFile, data, 0644)
	}

	log.Printf("[podcast-watch] Starting auto-podcast watcher for directories: %v, patterns: %v", dirs, patterns)

	// Queue for serial generation
	queue := make(chan string, 1000)

	// Worker: generate one at a time
	go func() {
		for relPath := range queue {
			log.Printf("[podcast-watch] Starting podcast generation for: %s", relPath)
			job := &podcastJob{Status: "generating", Progress: 0}
			a.podcastMu.Lock()
			a.podcastJobs[relPath] = job
			a.podcastMu.Unlock()

			a.generatePodcast(relPath, job)

			a.podcastMu.Lock()
			status := job.Status
			a.podcastMu.Unlock()

			if status == "done" {
				log.Printf("[podcast-watch] Completed podcast for: %s", relPath)
			} else {
				log.Printf("[podcast-watch] Failed podcast for %s: %s", relPath, job.Error)
			}
		}
	}()

	// Initial scan + periodic re-scan
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	scan := func() {
		processed := make(map[string]bool)

		// Process files in a given path with the needsGen logic
		processFile := func(path string, d fs.DirEntry) {
			if d.IsDir() {
				return
			}
			if !strings.HasSuffix(path, ".md") {
				return
			}
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || strings.HasSuffix(path, ".podcast-script.txt") {
				return
			}

			relPath, err := filepath.Rel(a.root, path)
			if err != nil {
				return
			}
			relPath = filepath.ToSlash(relPath)

			if processed[relPath] {
				return
			}
			processed[relPath] = true

			info, err := d.Info()
			if err != nil {
				return
			}
			modTime := info.ModTime().Unix()

			mp3Path := a.podcastMP3Path(relPath)
			mp3Info, mp3Err := os.Stat(mp3Path)

			needsGen := false
			if mp3Err != nil {
				if _, seen := known[relPath]; !seen {
					needsGen = true
					log.Printf("[podcast-watch] New file detected: %s", relPath)
				}
			} else {
				if modTime > mp3Info.ModTime().Unix() {
					needsGen = true
					log.Printf("[podcast-watch] File modified since last podcast: %s", relPath)
				}
			}

			known[relPath] = modTime

			if needsGen {
				a.podcastMu.Lock()
				existing, exists := a.podcastJobs[relPath]
				busy := exists && existing.Status == "generating"
				a.podcastMu.Unlock()

				if !busy {
					log.Printf("[podcast-watch] Queuing podcast generation for: %s", relPath)
					select {
					case queue <- relPath:
					default:
						log.Printf("[podcast-watch] Queue full, skipping: %s", relPath)
					}
				}
			}
		}

		// Scan watched directories
		for _, dir := range dirs {
			absDir := filepath.Join(a.root, dir)
			filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				processFile(path, d)
				return nil
			})
		}

		// Scan for glob pattern matches across entire root
		if len(patterns) > 0 {
			filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if !strings.HasSuffix(path, ".md") {
					return nil
				}
				base := filepath.Base(path)
				for _, pat := range patterns {
					matched, matchErr := filepath.Match(pat, base)
					if matchErr == nil && matched {
						log.Printf("[podcast-watch] Pattern %q matched: %s", pat, path)
						processFile(path, d)
						break
					}
				}
				return nil
			})
		}

		saveState()
	}

	scan()
	for range ticker.C {
		scan()
	}
}

func (a *app) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	relPath, err := sanitizeRelativePath(req.Path)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !isMarkdownFile(relPath) {
		http.Error(w, "only markdown files are supported", http.StatusBadRequest)
		return
	}
	fullPath, err := secureJoin(a.root, relPath)
	if err != nil {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	// Verify file exists before writing
	if _, err := os.Stat(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to stat file", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(fullPath, []byte(req.Content), 0644); err != nil {
		http.Error(w, "failed to write file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		OK bool `json:"ok"`
	}{OK: true})
}

func (a *app) handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	moved := 0
	for _, f := range req.Files {
		relPath, err := sanitizeRelativePath(f)
		if err != nil || !isMarkdownFile(relPath) {
			continue
		}
		srcAbs, err := secureJoin(a.root, relPath)
		if err != nil {
			continue
		}

		dirRel := filepath.Dir(relPath)
		fileName := filepath.Base(relPath)

		// Build .archive destination
		archiveDir := filepath.Join(a.root, dirRel, ".archive")
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			continue
		}
		dstAbs := filepath.Join(archiveDir, fileName)

		if err := os.Rename(srcAbs, dstAbs); err != nil {
			continue
		}

		// Remove tag and opened state from .mdviewer
		srcDirAbs := filepath.Join(a.root, filepath.FromSlash(dirRel))
		data, err := readMdviewerFile(srcDirAbs)
		if err == nil {
			delete(data.Tags, fileName)
			delete(data.Opened, fileName)
			_ = writeMdviewerFile(srcDirAbs, data)
		}
		moved++
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Moved int `json:"moved"`
	}{Moved: moved})
}

func searchFiles(root, query string) ([]searchResult, error) {
	lowerQuery := strings.ToLower(query)
	var results []searchResult

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isMarkdownFile(d.Name()) {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}

		text := string(content)
		lowerText := strings.ToLower(text)
		idx := strings.Index(lowerText, lowerQuery)
		if idx < 0 {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		// Extract a context snippet around the match
		contextStart := idx - 60
		if contextStart < 0 {
			contextStart = 0
		}
		contextEnd := idx + len(query) + 60
		if contextEnd > len(text) {
			contextEnd = len(text)
		}

		snippet := text[contextStart:contextEnd]
		// Clean up newlines in snippet
		snippet = strings.ReplaceAll(snippet, "\n", " ")
		snippet = strings.ReplaceAll(snippet, "\r", "")

		prefix := ""
		suffix := ""
		if contextStart > 0 {
			prefix = "…"
		}
		if contextEnd < len(text) {
			suffix = "…"
		}

		results = append(results, searchResult{
			Path:    filepath.ToSlash(rel),
			Context: prefix + snippet + suffix,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Path < results[j].Path
	})
	return results, nil
}

func listMarkdownFiles(root string) ([]string, error) {
	files := make([]string, 0, 16)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !isViewableFile(d.Name()) {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}

func isMarkdownFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".md" || ext == ".markdown"
}

func isPDFFile(path string) bool {
	return strings.ToLower(filepath.Ext(path)) == ".pdf"
}

func isHTMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".html" || ext == ".htm"
}

func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico", ".avif":
		return true
	default:
		return false
	}
}

// isLogFile reports whether the file is a log file that can be live-tailed.
func isLogFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".log", ".jsonl", ".ndjson":
		return true
	default:
		return false
	}
}

// isViewableFile reports whether the file can be shown in the viewer sidebar
// and content area: markdown, PDF, HTML, image, or log files.
func isViewableFile(path string) bool {
	return isMarkdownFile(path) || isPDFFile(path) || isHTMLFile(path) || isImageFile(path) || isLogFile(path)
}

func sanitizeRelativePath(path string) (string, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return "", errors.New("path is required")
	}
	clean = filepath.Clean(filepath.FromSlash(clean))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", errors.New("invalid path")
	}
	return filepath.ToSlash(clean), nil
}

// --- Podcast Player ---

type podcastEntry struct {
	Name       string  `json:"name"`
	Folder     string  `json:"folder"`
	Path       string  `json:"path"`
	Size       int64   `json:"size"`
	ModifiedAt int64   `json:"modifiedAt"`
	Duration   float64 `json:"duration,omitempty"`
}

func podcastProgressFile() string {
	return filepath.Join(mdviewerDataDir(), "podcast-progress.json")
}

func (a *app) handlePodcasts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, podcastsHTML)
}

func (a *app) handlePodcastList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var podcasts []podcastEntry
	filepath.WalkDir(a.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".podcast.mp3") {
			return nil
		}
		rel, _ := filepath.Rel(a.root, path)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		name := filepath.Base(rel)
		name = strings.TrimSuffix(name, ".podcast.mp3")
		folder := filepath.Dir(rel)
		if folder == "." {
			folder = ""
		}
		podcasts = append(podcasts, podcastEntry{
			Name:       name,
			Folder:     folder,
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().Unix(),
		})
		return nil
	})
	if podcasts == nil {
		podcasts = []podcastEntry{}
	}
	sort.Slice(podcasts, func(i, j int) bool {
		return podcasts[i].ModifiedAt > podcasts[j].ModifiedAt
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(podcasts)
}

func (a *app) handlePodcastProgress(w http.ResponseWriter, r *http.Request) {
	fp := podcastProgressFile()
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(fp)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{}")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		// Validate JSON
		var check map[string]interface{}
		if json.Unmarshal(body, &check) != nil {
			http.Error(w, "invalid json", 400)
			return
		}
		os.WriteFile(fp, body, 0644)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func podcastQueueFile() string {
	return filepath.Join(mdviewerDataDir(), "podcast-queue.json")
}

func (a *app) handlePodcastQueue(w http.ResponseWriter, r *http.Request) {
	fp := podcastQueueFile()
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(fp)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "[]")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		var check []interface{}
		if json.Unmarshal(body, &check) != nil {
			http.Error(w, "invalid json", 400)
			return
		}
		os.WriteFile(fp, body, 0644)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func secureJoin(root, rel string) (string, error) {
	joined := filepath.Join(root, filepath.FromSlash(rel))

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}

	relCheck, err := filepath.Rel(absRoot, absJoined)
	if err != nil {
		return "", err
	}
	if relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes root")
	}
	return absJoined, nil
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Markdown Viewer</title>
  <script src="https://cdn.jsdelivr.net/npm/marked/marked.min.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
  <style>
    :root {
      --bg: #0d1117;
      --panel: #161b22;
      --border: #30363d;
      --text: #c9d1d9;
      --muted: #8b949e;
      --link: #58a6ff;
      --code-bg: #161b22;
      --active: #1f6feb33;
      --sidebar-bg: #010409;
      --button-bg: #21262d;
      --button-hover: #30363d;
      --raw-code: #c9d1d9;
    }

    :root[data-theme="light"] {
      --bg: #f6f8fa;
      --panel: #ffffff;
      --border: #d0d7de;
      --text: #24292f;
      --muted: #57606a;
      --link: #0969da;
      --code-bg: #f6f8fa;
      --active: #0969da1a;
      --sidebar-bg: #ffffff;
      --button-bg: #f6f8fa;
      --button-hover: #eaeef2;
      --raw-code: #24292f;
    }

    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
      background: var(--bg);
      color: var(--text);
    }

    .app {
      display: grid;
      grid-template-columns: 300px minmax(0, 1fr);
      min-height: 100vh;
    }

    .app.sidebar-hidden {
      grid-template-columns: minmax(0, 1fr);
    }

    .app.sidebar-hidden .sidebar {
      display: none;
    }

    .sidebar {
      border-right: 1px solid var(--border);
      background: var(--sidebar-bg);
      padding: 16px;
      overflow-y: auto;
      position: sticky;
      top: 0;
      height: 100vh;
    }

    .sidebar h1 {
      margin: 0;
      font-size: 16px;
      font-weight: 700;
    }

    .root-path {
      margin-top: 6px;
      font-size: 12px;
      color: var(--muted);
      word-break: break-all;
    }

    .files {
      margin-top: 14px;
      display: flex;
      flex-direction: column;
      gap: 0;
      user-select: none;
    }

    .tree-item {
      display: flex;
      align-items: center;
      width: 100%;
      border: none;
      background: transparent;
      color: var(--text);
      text-align: left;
      padding: 3px 0;
      padding-right: 8px;
      cursor: pointer;
      font-size: 13px;
      font-family: inherit;
      line-height: 22px;
      height: 22px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      border-radius: 0;
    }

    .tree-item:hover { background: var(--panel); }
    .tree-item.active {
      background: var(--active);
    }

    .tree-chevron {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 16px;
      min-width: 16px;
      height: 22px;
      font-size: 10px;
      color: var(--muted);
      transition: transform 0.1s ease;
    }

    .tree-chevron.expanded {
      transform: rotate(90deg);
    }

    .tree-chevron.placeholder {
      visibility: hidden;
    }

    .tree-icon {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 16px;
      min-width: 16px;
      height: 22px;
      margin-right: 4px;
      font-size: 14px;
    }

    .tree-icon.folder-icon { color: #e8a64e; }
    .tree-icon.file-icon { color: var(--muted); }

    .tree-label {
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .tree-children {
      display: flex;
      flex-direction: column;
    }

    .tree-children.collapsed {
      display: none;
    }

    .search-box {
      margin-top: 12px;
      position: relative;
    }

    .search-box input {
      width: 100%;
      padding: 6px 30px 6px 10px;
      border: 1px solid var(--border);
      border-radius: 6px;
      background: var(--panel);
      color: var(--text);
      font-size: 13px;
      font-family: inherit;
      outline: none;
    }

    .search-box input:focus {
      border-color: var(--link);
    }

    .search-box input::placeholder {
      color: var(--muted);
    }

    .search-clear {
      position: absolute;
      right: 6px;
      top: 50%;
      transform: translateY(-50%);
      border: none;
      background: transparent;
      color: var(--muted);
      cursor: pointer;
      font-size: 14px;
      padding: 0 4px;
      line-height: 1;
      display: none;
    }

    .search-clear:hover { color: var(--text); }

    .search-result-item {
      display: block;
      width: 100%;
      border: none;
      background: transparent;
      color: var(--text);
      text-align: left;
      padding: 8px 8px;
      cursor: pointer;
      font-size: 13px;
      font-family: inherit;
      border-bottom: 1px solid var(--border);
    }

    .search-result-item:last-child { border-bottom: none; }
    .search-result-item:hover { background: var(--panel); }
    .search-result-item.active { background: var(--active); }

    .search-result-path {
      font-weight: 600;
      margin-bottom: 3px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .search-result-context {
      font-size: 12px;
      color: var(--muted);
      line-height: 1.4;
      word-break: break-word;
    }

    .search-result-context mark {
      background: #f0b429;
      color: #1a1a1a;
      border-radius: 2px;
      padding: 0 1px;
    }

    .search-info {
      font-size: 12px;
      color: var(--muted);
      padding: 8px 0;
    }

    mark.search-highlight {
      background: #f0b429;
      color: #1a1a1a;
      border-radius: 2px;
      padding: 0 1px;
      scroll-margin-top: 80px;
    }

    .main {
      padding: 24px;
      overflow-y: auto;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 16px;
      gap: 12px;
    }

    .header-actions {
      display: flex;
      gap: 8px;
    }

    .header h2 {
      margin: 0;
      font-size: 20px;
      font-weight: 600;
      overflow-wrap: anywhere;
    }

    .muted {
      color: var(--muted);
      font-size: 13px;
      margin-top: 4px;
    }

    .btn {
      border: 1px solid var(--border);
      border-radius: 6px;
      background: var(--button-bg);
      color: var(--text);
      padding: 6px 12px;
      cursor: pointer;
      font-size: 13px;
    }

    .btn:hover { background: var(--button-hover); }
    .btn:disabled {
      opacity: 0.4;
      cursor: not-allowed;
    }
    .btn:disabled:hover { background: var(--button-bg); }

    .viewer {
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--panel);
      padding: 24px;
    }

    .hidden { display: none; }

    .markdown-body {
      line-height: 1.7;
      color: var(--text);
    }
    .markdown-body h1, .markdown-body h2, .markdown-body h3 {
      border-bottom: 1px solid var(--border);
      padding-bottom: .3em;
      margin-top: 24px;
      margin-bottom: 16px;
    }
    .markdown-body h1 { font-size: 2em; }
    .markdown-body h2 { font-size: 1.5em; }
    .markdown-body h3 { font-size: 1.25em; }
    .markdown-body p, .markdown-body ul, .markdown-body ol { margin: 0 0 16px; }
    .markdown-body code {
      background: var(--code-bg);
      border: 1px solid var(--border);
      padding: 0.15em 0.35em;
      border-radius: 6px;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 85%;
    }
    .markdown-body pre {
      background: var(--code-bg);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 12px;
      overflow-x: auto;
      margin-bottom: 16px;
    }
    .markdown-body pre code {
      border: 0;
      padding: 0;
      background: transparent;
    }
    .markdown-body blockquote {
      border-left: 4px solid #3b82f6;
      margin: 0 0 16px;
      padding: 0 12px;
      color: var(--muted);
    }
    .markdown-body table {
      border-collapse: collapse;
      width: 100%;
      margin-bottom: 16px;
      display: block;
      overflow-x: auto;
    }
    .markdown-body th, .markdown-body td {
      border: 1px solid var(--border);
      padding: 6px 10px;
      text-align: left;
    }
    .markdown-body a {
      color: var(--link);
      text-decoration: none;
    }
    .markdown-body a:hover { text-decoration: underline; }

    .markdown-body img {
      max-width: 100%;
      height: auto;
      display: block;
      margin: 12px 0;
      border-radius: 6px;
    }
    .markdown-body a img { display: inline-block; }
    .markdown-body td img, .markdown-body th img { display: inline-block; margin: 0; }

    .mermaid-block {
      background: #fff;
      border-radius: 8px;
      padding: 16px;
      margin: 16px 0;
      overflow-x: auto;
    }

    #raw-code {
      color: var(--raw-code);
      white-space: pre-wrap;
      word-break: break-word;
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 13px;
    }

    .tree-tag {
      margin-left: auto;
      font-size: 12px;
      line-height: 1;
      flex-shrink: 0;
      padding-left: 4px;
    }

    .tag-menu {
      position: fixed;
      z-index: 1000;
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 8px;
      padding: 4px 0;
      min-width: 160px;
      box-shadow: 0 8px 24px rgba(0,0,0,0.3);
    }

    .tag-menu-item {
      display: flex;
      align-items: center;
      gap: 8px;
      width: 100%;
      border: none;
      background: transparent;
      color: var(--text);
      text-align: left;
      padding: 6px 12px;
      cursor: pointer;
      font-size: 13px;
      font-family: inherit;
    }

    .tag-menu-item:hover { background: var(--active); }

    .tag-menu-divider {
      height: 1px;
      background: var(--border);
      margin: 4px 0;
    }

    .header-tags {
      display: flex;
      gap: 4px;
      flex-wrap: wrap;
      margin-top: 6px;
    }

    .header-tag-btn {
      border: 1px solid var(--border);
      border-radius: 12px;
      background: var(--button-bg);
      color: var(--muted);
      padding: 2px 10px;
      cursor: pointer;
      font-size: 12px;
      font-family: inherit;
      line-height: 1.5;
      transition: background 0.15s, border-color 0.15s;
    }

    .header-tag-btn:hover { background: var(--button-hover); }

    .header-tag-btn.active {
      background: var(--active);
      border-color: var(--link);
      color: var(--text);
    }

    .tag-filter-wrapper {
      position: relative;
      margin-top: 6px;
    }

    .tag-filter-btn {
      display: flex;
      align-items: center;
      gap: 4px;
      width: 100%;
      padding: 5px 10px;
      border: 1px solid var(--border);
      border-radius: 6px;
      background: var(--panel);
      color: var(--muted);
      font-size: 12px;
      font-family: inherit;
      cursor: pointer;
      text-align: left;
    }

    .tag-filter-btn:hover { border-color: var(--link); }

    .tag-filter-dropdown {
      position: absolute;
      top: 100%;
      left: 0;
      right: 0;
      z-index: 100;
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 6px;
      margin-top: 2px;
      padding: 4px 0;
      box-shadow: 0 4px 12px rgba(0,0,0,0.2);
    }

    .tag-filter-option {
      display: flex;
      align-items: center;
      gap: 6px;
      width: 100%;
      border: none;
      background: transparent;
      color: var(--text);
      text-align: left;
      padding: 4px 10px;
      cursor: pointer;
      font-size: 12px;
      font-family: inherit;
    }

    .tag-filter-option:hover { background: var(--active); }

    .tag-filter-option input[type="checkbox"] {
      margin: 0;
      accent-color: var(--link);
    }

    #edit-textarea {
      width: 100%;
      min-height: 500px;
      padding: 16px;
      border: 1px solid var(--border);
      border-radius: 8px;
      background: var(--panel);
      color: var(--text);
      font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
      font-size: 14px;
      line-height: 1.6;
      resize: vertical;
      outline: none;
      tab-size: 2;
    }

    #edit-textarea:focus {
      border-color: var(--link);
    }

    .save-status {
      font-size: 12px;
      color: var(--muted);
      margin-left: 8px;
    }

    .markdown-body input[type="checkbox"] {
      cursor: pointer;
    }

    /* Sidebar backdrop (mobile overlay) */
    .sidebar-backdrop {
      display: none;
      position: fixed;
      inset: 0;
      background: rgba(0,0,0,0.5);
      z-index: 199;
    }

    /* Hamburger button — hidden on desktop */
    .mobile-menu-btn {
      display: none;
      border: 1px solid var(--border);
      border-radius: 6px;
      background: var(--button-bg);
      color: var(--text);
      padding: 6px 10px;
      cursor: pointer;
      font-size: 18px;
      line-height: 1;
      font-family: inherit;
    }
    .mobile-menu-btn:hover { background: var(--button-hover); }

    @media (max-width: 768px) {
      /* Stack layout: sidebar is an overlay, main fills full width */
      .app {
        display: block;
      }
      .app.sidebar-hidden {
        display: block;
      }

      /* Sidebar becomes a fixed drawer */
      .sidebar {
        position: fixed;
        left: 0;
        top: 0;
        height: 100vh;
        width: 280px;
        z-index: 200;
        transform: translateX(0);
        transition: transform 0.25s ease;
        box-shadow: 4px 0 20px rgba(0,0,0,0.4);
      }

      /* When hidden on mobile: slide out instead of display:none */
      .app.sidebar-hidden .sidebar {
        display: block !important;
        transform: translateX(-100%);
      }

      /* Backdrop visible when sidebar is open on mobile */
      .app:not(.sidebar-hidden) .sidebar-backdrop {
        display: block;
      }

      /* Show hamburger, hide desktop sidebar toggle */
      .mobile-menu-btn { display: inline-flex; align-items: center; }
      #toggle-sidebar-btn { display: none; }

      .main { padding: 12px; }
      .viewer { padding: 12px; }

      /* Larger touch targets in file tree */
      .tree-item {
        height: 36px;
        line-height: 36px;
        padding-top: 0;
        padding-bottom: 0;
      }
      .tree-chevron, .tree-icon { height: 36px; }

      /* Header wraps on small screens */
      .header { flex-wrap: wrap; }
      .header-actions {
        flex-wrap: wrap;
        gap: 6px;
        width: 100%;
      }
      .header h2 { font-size: 16px; }

      /* Shrink buttons a bit */
      .btn { padding: 5px 8px; font-size: 12px; }
    }

    /* --- Log viewer --- */
    .log-toolbar {
      display: flex; flex-wrap: wrap; align-items: center; gap: 8px;
      padding: 8px 0; margin-bottom: 8px;
      border-bottom: 1px solid var(--border); position: sticky; top: 0;
      background: var(--bg); z-index: 5;
    }
    .log-toolbar input.log-filter {
      flex: 1; min-width: 160px; padding: 6px 10px;
      border: 1px solid var(--border); border-radius: 6px;
      background: var(--bg); color: var(--fg); font-size: 13px;
    }
    .log-toolbar .log-stat { font-size: 12px; color: var(--muted); white-space: nowrap; }
    .log-live-on { background: #238636 !important; border-color: #238636 !important; color: #fff !important; }
    .log-output {
      font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      font-size: 12.5px; line-height: 1.5; white-space: pre-wrap; word-break: break-word;
      margin: 0; max-height: calc(100vh - 220px); overflow: auto;
      background: var(--code-bg, rgba(127,127,127,0.08)); border-radius: 6px; padding: 10px;
    }
    .log-line.log-hit { background: rgba(255, 221, 0, 0.18); }
    .log-table-wrap { max-height: calc(100vh - 220px); overflow: auto; border: 1px solid var(--border); border-radius: 6px; }
    table.log-table { border-collapse: collapse; width: 100%; font-size: 12.5px; table-layout: fixed; }
    table.log-table th, table.log-table td {
      border: 1px solid var(--border); padding: 4px 8px; text-align: left;
      vertical-align: top; font-family: ui-monospace, Menlo, Consolas, monospace;
      overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    }
    table.log-table td { white-space: pre-wrap; overflow-wrap: anywhere; }
    table.log-table th {
      position: sticky; top: 0; background: var(--bg); z-index: 2;
      position: relative; user-select: none;
    }
    table.log-table thead tr.log-filter-row th { top: 28px; padding: 2px; }
    table.log-table thead tr.log-filter-row input.log-col-filter {
      width: 100%; box-sizing: border-box; padding: 2px 5px; font-size: 11px;
      border: 1px solid var(--border); border-radius: 4px; background: var(--bg); color: var(--fg);
      font-family: inherit;
    }
    .log-th-label { display: inline-block; max-width: calc(100% - 8px); overflow: hidden; text-overflow: ellipsis; vertical-align: bottom; }
    .log-col-resizer {
      position: absolute; top: 0; right: 0; width: 7px; height: 100%;
      cursor: col-resize; z-index: 3;
    }
    .log-col-resizer:hover { background: rgba(88,166,255,0.4); }
    table.log-table tr.log-raw-row td { font-style: italic; color: var(--muted); white-space: pre-wrap; }
    .log-badge { display: inline-block; padding: 1px 7px; border-radius: 10px; font-size: 11px; font-weight: 600; color: #fff; }
    .log-lvl-error, .log-lvl-fatal, .log-lvl-critical { background: #e74c3c; }
    .log-lvl-warn, .log-lvl-warning { background: #e67e22; }
    .log-lvl-info { background: #3498db; }
    .log-lvl-debug, .log-lvl-trace { background: #7f8c8d; }

    /* Dropdown menus (columns / views) */
    .log-dropdown { position: relative; display: inline-block; }
    .log-menu {
      position: absolute; top: calc(100% + 4px); left: 0; z-index: 50;
      min-width: 220px; max-height: 60vh; overflow: auto;
      background: var(--bg); border: 1px solid var(--border); border-radius: 8px;
      box-shadow: 0 8px 24px rgba(0,0,0,0.25); padding: 6px;
    }
    .log-menu-title { font-size: 11px; font-weight: 700; color: var(--muted); text-transform: uppercase; padding: 4px 6px; }
    .log-menu-empty { font-size: 12px; color: var(--muted); padding: 6px; }
    .log-menu-row {
      display: flex; align-items: center; gap: 6px; padding: 4px 6px;
      font-size: 13px; border-radius: 5px; cursor: pointer;
    }
    .log-menu-row:hover { background: rgba(127,127,127,0.12); }
    .log-menu-actions { display: flex; gap: 6px; padding: 6px; border-top: 1px solid var(--border); margin-top: 4px; }
    .log-view-row { justify-content: space-between; }
    .log-view-apply { flex: 1; }
    .log-view-scope { font-size: 10px; color: var(--muted); margin-left: 4px; }
    .log-view-active { background: rgba(35,134,54,0.18); }
    .log-view-del { color: #e74c3c; font-weight: 700; padding: 0 6px; }
    .log-view-del:hover { background: rgba(231,76,60,0.15); border-radius: 4px; }
  </style>
</head>
<body>
  <div class="sidebar-backdrop" id="sidebar-backdrop"></div>
  <div class="app">
    <aside class="sidebar">
      <h1>Markdown Files</h1>
      <div class="root-path">{{ .Root }}</div>
      <div class="search-box">
        <input type="text" id="search-input" placeholder="Search in files…" autocomplete="off" />
        <button class="search-clear" id="search-clear" type="button">&times;</button>
      </div>
      <div class="tag-filter-wrapper">
        <button class="tag-filter-btn" id="tag-filter-btn" type="button">🏷️ Filter by tags ▾</button>
        <div class="tag-filter-dropdown hidden" id="tag-filter-dropdown"></div>
      </div>
      <div style="margin-top:6px;">
        <button class="tag-filter-btn" id="sort-toggle-btn" type="button">📁 Sort: Name</button>
      </div>
      <div class="files" id="file-list">
        <div class="muted">Loading files…</div>
      </div>
    </aside>
    <main class="main">
      <div class="header">
        <div>
          <h2 id="file-name">Select a markdown file</h2>
          <div class="muted">GitHub-like markdown preview with Mermaid support</div>
          <div class="header-tags hidden" id="header-tags"></div>
        </div>
        <div class="header-actions">
          <button class="mobile-menu-btn" id="mobile-menu-btn" type="button" aria-label="Open sidebar">&#9776;</button>
          <button id="prev-file-btn" class="btn nav-btn hidden" type="button" title="Previous file">&#9664; Prev</button>
          <button id="next-file-btn" class="btn nav-btn hidden" type="button" title="Next file">Next &#9654;</button>
          <button id="toggle-sidebar-btn" class="btn" type="button">Hide Sidebar</button>
          <button id="theme-toggle-btn" class="btn" type="button">Light Mode</button>
          <button id="toggle-raw-btn" class="btn hidden" type="button">Show Raw</button>
          <button id="edit-btn" class="btn hidden" type="button">✏️ Edit</button>
          <button id="save-btn" class="btn hidden" type="button" style="background:#238636;border-color:#238636;color:#fff;">💾 Save</button>
          <button id="cancel-edit-btn" class="btn hidden" type="button">Cancel</button>
          <button id="archive-btn" class="btn" type="button" title="Move all ARCHIVE-tagged files to .archive folder">&#128230; Archive</button>
          <button id="podcast-btn" class="btn hidden" type="button" title="Generate podcast from this document">🎙️ Podcast</button>
          <span id="podcast-status" class="hidden" style="margin-left:8px; font-size:13px; color:#8b949e;"></span>
          <audio id="podcast-player" class="hidden" controls style="margin-left:8px; height:30px; vertical-align:middle;"></audio>
        </div>
      </div>
      <section class="viewer">
        <div id="rendered-content" class="markdown-body">
          <div class="muted">Pick a file from the left to render it.</div>
        </div>
        <div id="raw-content" class="hidden">
          <pre><code id="raw-code"></code></pre>
        </div>
      </section>
    </main>
  </div>

  <script>
    const INITIAL_FILE = {{ printf "%q" .InitialFile }};
    const appEl = document.querySelector('.app');
    const fileListEl = document.getElementById('file-list');
    const fileNameEl = document.getElementById('file-name');
    const renderedEl = document.getElementById('rendered-content');
    const rawContainerEl = document.getElementById('raw-content');
    const rawCodeEl = document.getElementById('raw-code');
    const toggleSidebarBtn = document.getElementById('toggle-sidebar-btn');
    const mobileMenuBtn = document.getElementById('mobile-menu-btn');
    const sidebarBackdrop = document.getElementById('sidebar-backdrop');
    const themeToggleBtn = document.getElementById('theme-toggle-btn');
    const toggleRawBtn = document.getElementById('toggle-raw-btn');
    const prevFileBtn = document.getElementById('prev-file-btn');
    const nextFileBtn = document.getElementById('next-file-btn');
    const searchInput = document.getElementById('search-input');
    const searchClear = document.getElementById('search-clear');
    const headerTagsEl = document.getElementById('header-tags');
    const tagFilterBtn = document.getElementById('tag-filter-btn');
    const tagFilterDropdown = document.getElementById('tag-filter-dropdown');
    const archiveBtn = document.getElementById('archive-btn');
    const sortToggleBtn = document.getElementById('sort-toggle-btn');
    const STORAGE_THEME_KEY = 'mdviewer-theme';

    // --- File type helpers (keep in sync with backend) ---
    const IMAGE_EXTS = ['.png', '.jpg', '.jpeg', '.gif', '.webp', '.svg', '.bmp', '.ico', '.avif'];
    function isPdfPath(p) { return p.toLowerCase().endsWith('.pdf'); }
    function isHtmlPath(p) { const l = p.toLowerCase(); return l.endsWith('.html') || l.endsWith('.htm'); }
    function isImagePath(p) { const l = p.toLowerCase(); return IMAGE_EXTS.some(e => l.endsWith(e)); }
    const LOG_EXTS = ['.log', '.jsonl', '.ndjson'];
    function isLogPath(p) { const l = p.toLowerCase(); return LOG_EXTS.some(e => l.endsWith(e)); }
    function isMarkdownPath(p) { const l = p.toLowerCase(); return l.endsWith('.md') || l.endsWith('.markdown'); }
    function mediaUrlFor(p) { return '/api/media/' + p.split('/').map(s => encodeURIComponent(s)).join('/'); }

    let files = [];
    let activeFile = '';
    let rawContent = '';
    let showingRaw = false;
    let sidebarHidden = false;
    let searchTimer = null;
    let searchMode = false;
    let baseFolderPath = '';
    let fileTags = {};
    let fileOpened = {};
    let activeTagFilters = new Set();
    let fileMeta = {};  // path -> { modifiedAt, createdAt }
    let sortMode = 'name'; // 'name' or 'modified'

    const TAG_ICONS = {
      'DONE': '\u2705',
      'IN-PROGRESS': '\uD83D\uDD04',
      'NEXT': '\u23ED\uFE0F',
      'IMPORTANT': '\u2B50',
      'REVISIT': '\uD83D\uDD01',
      'ARCHIVE': '\uD83D\uDCE6',
      'UNREAD': '\uD83D\uDCD5'
    };
    const TAG_LIST = ['DONE', 'IN-PROGRESS', 'NEXT', 'IMPORTANT', 'REVISIT', 'ARCHIVE'];
    const ALL_FILTER_TAGS = ['UNREAD', ...TAG_LIST];

    if (window.mermaid) {
      window.mermaid.initialize({ startOnLoad: false, securityLevel: 'loose', theme: 'neutral' });
    }

    loadThemePreference();

    toggleSidebarBtn.addEventListener('click', () => {
      applySidebarVisibility(!sidebarHidden, true);
    });

    mobileMenuBtn.addEventListener('click', () => {
      applySidebarVisibility(false, true);
    });

    sidebarBackdrop.addEventListener('click', () => {
      applySidebarVisibility(true, true);
    });

    themeToggleBtn.addEventListener('click', () => {
      const currentTheme = document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
      const nextTheme = currentTheme === 'dark' ? 'light' : 'dark';
      applyTheme(nextTheme);
      window.localStorage.setItem(STORAGE_THEME_KEY, nextTheme);
    });

    toggleRawBtn.addEventListener('click', () => {
      showingRaw = !showingRaw;
      renderedEl.classList.toggle('hidden', showingRaw);
      rawContainerEl.classList.toggle('hidden', !showingRaw);
      toggleRawBtn.textContent = showingRaw ? 'Show Rendered' : 'Show Raw';
    });

    prevFileBtn.addEventListener('click', () => navigateFile(-1));
    nextFileBtn.addEventListener('click', () => navigateFile(1));

    archiveBtn.addEventListener('click', async () => {
      const archiveFiles = files.filter(f => (fileTags[f] || []).includes('ARCHIVE'));
      if (archiveFiles.length === 0) {
        alert('No files tagged with ARCHIVE.');
        return;
      }
      if (!confirm('Move ' + archiveFiles.length + ' file(s) tagged ARCHIVE to .archive folder?')) return;
      try {
        const resp = await fetch('/api/archive', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ files: archiveFiles })
        });
        if (!resp.ok) { const t = await resp.text(); throw new Error(t); }
        const result = await resp.json();
        // Refresh file list and tags
        const [filesResp, tagsResp] = await Promise.all([fetch('/api/files'), fetch('/api/tags')]);
        if (filesResp.ok) {
          const p = await filesResp.json();
          let allFiles = p.files || [];
          if (baseFolderPath) {
            const prefix = baseFolderPath + '/';
            files = allFiles.filter(f => f.startsWith(prefix) || f === baseFolderPath);
          } else {
            files = allFiles;
          }
        }
        if (tagsResp.ok) {
          const tp = await tagsResp.json();
          fileTags = tp.tags || {};
          fileOpened = tp.opened || {};
        }
        renderFileList();
        if (activeFile && !files.includes(activeFile)) {
          if (files.length > 0) {
            await openFile(files[0], true);
          } else {
            activeFile = '';
            fileNameEl.textContent = 'Select a markdown file';
            renderedEl.innerHTML = '<div class="muted">No markdown files found.</div>';
            renderHeaderTags();
          }
        }
        alert('Archived ' + result.moved + ' file(s).');
      } catch (err) {
        alert('Archive failed: ' + err.message);
      }
    });

    sortToggleBtn.addEventListener('click', () => {
      sortMode = sortMode === 'name' ? 'modified' : 'name';
      sortToggleBtn.textContent = sortMode === 'name' ? '📁 Sort: Name' : '🕐 Sort: Modified';
      if (!searchMode) renderFileList();
      // Update URL
      const url = new URL(window.location.href);
      if (sortMode === 'modified') {
        url.searchParams.set('sort', 'modified');
      } else {
        url.searchParams.delete('sort');
      }
      window.history.replaceState(window.history.state, '', url);
    });

    searchInput.addEventListener('input', () => {
      const query = searchInput.value.trim();
      searchClear.style.display = query ? 'block' : 'none';
      clearTimeout(searchTimer);
      if (!query) {
        searchMode = false;
        renderFileList();
        return;
      }
      searchTimer = setTimeout(() => performSearch(query), 250);
    });

    searchClear.addEventListener('click', () => {
      searchInput.value = '';
      searchClear.style.display = 'none';
      searchMode = false;
      renderFileList();
    });

    tagFilterBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      const isHidden = tagFilterDropdown.classList.toggle('hidden');
      if (!isHidden) {
        renderTagFilterDropdown();
        setTimeout(() => document.addEventListener('click', closeTagFilter, { once: true }), 0);
      }
    });

    function closeTagFilter() {
      tagFilterDropdown.classList.add('hidden');
    }

    function renderTagFilterDropdown() {
      tagFilterDropdown.innerHTML = '';
      for (const tag of ALL_FILTER_TAGS) {
        const label = document.createElement('label');
        label.className = 'tag-filter-option';
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = activeTagFilters.has(tag);
        cb.addEventListener('change', (e) => {
          e.stopPropagation();
          if (cb.checked) {
            activeTagFilters.add(tag);
          } else {
            activeTagFilters.delete(tag);
          }
          updateTagFilterBtnLabel();
          if (!searchMode) renderFileList();
        });
        label.appendChild(cb);
        label.appendChild(document.createTextNode(TAG_ICONS[tag] + ' ' + tag));
        label.addEventListener('click', (e) => e.stopPropagation());
        tagFilterDropdown.appendChild(label);
      }
    }

    function updateTagFilterBtnLabel() {
      if (activeTagFilters.size === 0) {
        tagFilterBtn.textContent = '\uD83C\uDFF7\uFE0F Filter by tags \u25BE';
      } else {
        const icons = [...activeTagFilters].map(t => TAG_ICONS[t] || t).join(' ');
        tagFilterBtn.textContent = '\uD83C\uDFF7\uFE0F ' + icons + ' \u25BE';
      }
    }

    function getEffectiveTags(filePath) {
      const tags = (fileTags[filePath] || []).slice();
      if (!fileOpened[filePath]) {
        tags.push('UNREAD');
      }
      return tags;
    }

    function fileMatchesTagFilter(filePath) {
      if (activeTagFilters.size === 0) return true;
      const tags = getEffectiveTags(filePath);
      for (const f of activeTagFilters) {
        if (tags.includes(f)) return true;
      }
      return false;
    }

    function navigateFile(direction) {
      if (!files.length || !activeFile) return;
      const idx = files.indexOf(activeFile);
      const next = idx + direction;
      if (next >= 0 && next < files.length) {
        openFile(files[next], true);
      }
    }

    function updateNavButtons() {
      if (!files.length || !activeFile) {
        prevFileBtn.classList.add('hidden');
        nextFileBtn.classList.add('hidden');
        return;
      }
      const idx = files.indexOf(activeFile);
      prevFileBtn.classList.remove('hidden');
      nextFileBtn.classList.remove('hidden');
      prevFileBtn.disabled = idx <= 0;
      nextFileBtn.disabled = idx >= files.length - 1;
    }

    window.addEventListener('popstate', () => {
      const params = new URLSearchParams(window.location.search);
      applySidebarVisibility(isFullscreenMode(params), false);
      const candidate = params.get('file');
      if (candidate && files.includes(candidate)) {
        openFile(candidate, false);
      }
    });

    init();

    async function init() {
      try {
        const params = new URLSearchParams(window.location.search);
        const rawBase = (params.get('baseFolderPath') || '').replace(/\/+$/, '').replace(/^\/+/, '');
        baseFolderPath = rawBase;

        // Read sort preference from URL
        if (params.get('sort') === 'modified') {
          sortMode = 'modified';
          sortToggleBtn.textContent = '🕐 Sort: Modified';
        }

        const [filesResp, tagsResp] = await Promise.all([
          fetch('/api/files'),
          fetch('/api/tags')
        ]);
        if (!filesResp.ok) throw new Error('failed to list files');
        const payload = await filesResp.json();
        let allFiles = payload.files || [];

        // Store file modification times
        if (payload.meta) {
          for (const m of payload.meta) {
            fileMeta[m.path] = { modifiedAt: m.modifiedAt, createdAt: m.createdAt };
          }
        }

        if (tagsResp.ok) {
          const tagsPayload = await tagsResp.json();
          fileTags = tagsPayload.tags || {};
          fileOpened = tagsPayload.opened || {};
        }

        if (baseFolderPath) {
          const prefix = baseFolderPath + '/';
          files = allFiles.filter(f => f.startsWith(prefix) || f === baseFolderPath);
        } else {
          files = allFiles;
        }
        renderFileList();
        _lastFileHash = fileListHash(files) + JSON.stringify(Object.values(fileMeta));

        // On mobile, default to sidebar-hidden unless URL explicitly shows sidebar
        const mobileDefault = window.innerWidth <= 768 && params.get('sidebar') === null;
        applySidebarVisibility(mobileDefault || isFullscreenMode(params), false);
        const requested = params.get('file') || INITIAL_FILE;
        if (requested && files.includes(requested)) {
          await openFile(requested, false);
        } else if (files.length > 0) {
          await openFile(files[0], false);
        } else {
          renderedEl.innerHTML = '<div class="muted">No markdown files found in the configured root.</div>';
        }
      } catch (err) {
        renderedEl.innerHTML = '<div class="muted">Failed to load markdown files.</div>';
      }
    }

    // Live-update sidebar: poll /api/files every 5s, re-render only if changed
    let _lastFileHash = '';
    function fileListHash(arr) { return arr.join('\n'); }
    async function pollFiles() {
      try {
        const [filesResp, tagsResp] = await Promise.all([fetch('/api/files'), fetch('/api/tags')]);
        if (!filesResp.ok) return;
        const payload = await filesResp.json();
        let allFiles = payload.files || [];
        if (payload.meta) {
          for (const m of payload.meta) {
            fileMeta[m.path] = { modifiedAt: m.modifiedAt, createdAt: m.createdAt };
          }
        }
        if (baseFolderPath) {
          const prefix = baseFolderPath + '/';
          allFiles = allFiles.filter(f => f.startsWith(prefix) || f === baseFolderPath);
        }
        const h = fileListHash(allFiles) + JSON.stringify(payload.meta || []);
        if (h !== _lastFileHash) {
          _lastFileHash = h;
          files = allFiles;
          if (tagsResp.ok) {
            const tp = await tagsResp.json();
            fileTags = tp.tags || {};
            fileOpened = tp.opened || {};
          }
          if (!searchMode) renderFileList();
        }
      } catch (_) {}
    }
    setInterval(pollFiles, 5000);

    function buildTree(filePaths) {
      const root = { name: '', children: {}, files: [] };
      for (const fp of filePaths) {
        const parts = fp.split('/');
        let node = root;
        for (let i = 0; i < parts.length - 1; i++) {
          const dir = parts[i];
          if (!node.children[dir]) {
            node.children[dir] = { name: dir, children: {}, files: [] };
          }
          node = node.children[dir];
        }
        node.files.push({ name: parts[parts.length - 1], path: fp });
      }
      return root;
    }

    function relativeTime(ms) {
      const diff = Date.now() - ms;
      const secs = Math.floor(diff / 1000);
      if (secs < 60) return 'now';
      const mins = Math.floor(secs / 60);
      if (mins < 60) return mins + 'm';
      const hrs = Math.floor(mins / 60);
      if (hrs < 24) return hrs + 'h';
      const days = Math.floor(hrs / 24);
      if (days < 30) return days + 'd';
      const months = Math.floor(days / 30);
      return months + 'mo';
    }

    function getNewestModTime(node, pathPrefix) {
      let newest = 0;
      for (const file of node.files) {
        // file.path is already the full path from root
        const meta = fileMeta[file.path];
        if (meta && meta.modifiedAt > newest) newest = meta.modifiedAt;
      }
      for (const dirName of Object.keys(node.children)) {
        const childPrefix = pathPrefix ? pathPrefix + '/' + dirName : dirName;
        const childNewest = getNewestModTime(node.children[dirName], childPrefix);
        if (childNewest > newest) newest = childNewest;
      }
      return newest;
    }

    function renderTreeNode(node, depth, container, pathPrefix) {
      if (sortMode === 'modified') {
        // Interleave dirs and files, sorted by newest mod time
        const items = [];
        for (const dirName of Object.keys(node.children)) {
          const dirPrefix = pathPrefix ? pathPrefix + '/' + dirName : dirName;
          items.push({ type: 'dir', name: dirName, modTime: getNewestModTime(node.children[dirName], dirPrefix) });
        }
        for (const file of node.files) {
          // file.path is already full path
          items.push({ type: 'file', file: file, modTime: (fileMeta[file.path] || {}).modifiedAt || 0 });
        }
        items.sort((a, b) => b.modTime - a.modTime);

        for (const item of items) {
          if (item.type === 'dir') {
            renderDirNode(item.name, node.children[item.name], depth, container, pathPrefix);
          } else {
            renderFileNode(item.file, depth, container, pathPrefix);
          }
        }
      } else {
        const sortedDirs = Object.keys(node.children).sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' }));
        const sortedFiles = node.files.slice().sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }));
        for (const dirName of sortedDirs) {
          renderDirNode(dirName, node.children[dirName], depth, container, pathPrefix);
        }
        for (const file of sortedFiles) {
          renderFileNode(file, depth, container, pathPrefix);
        }
      }
    }

    function renderDirNode(dirName, child, depth, container, pathPrefix) {
        const folderBtn = document.createElement('button');
        folderBtn.className = 'tree-item';
        folderBtn.type = 'button';
        folderBtn.style.paddingLeft = (depth * 16) + 'px';

        const chevron = document.createElement('span');
        chevron.className = 'tree-chevron';
        chevron.innerHTML = '&#9654;';

        const icon = document.createElement('span');
        icon.className = 'tree-icon folder-icon';
        icon.innerHTML = '&#128194;';

        const label = document.createElement('span');
        label.className = 'tree-label';
        label.textContent = dirName;

        folderBtn.appendChild(chevron);
        folderBtn.appendChild(icon);
        folderBtn.appendChild(label);

        // Show relative time on folders in modified sort mode
        if (sortMode === 'modified') {
          const dirPrefix = pathPrefix ? pathPrefix + '/' + dirName : dirName;
          const newest = getNewestModTime(child, dirPrefix);
          if (newest > 0) {
            const timeSpan = document.createElement('span');
            timeSpan.className = 'tree-tag';
            timeSpan.style.color = 'var(--muted)';
            timeSpan.style.fontSize = '11px';
            timeSpan.textContent = relativeTime(newest);
            timeSpan.title = new Date(newest).toLocaleString();
            folderBtn.appendChild(timeSpan);
          }
        }

        container.appendChild(folderBtn);

        const childContainer = document.createElement('div');
        childContainer.className = 'tree-children collapsed';
        container.appendChild(childContainer);

        folderBtn.addEventListener('click', () => {
          const isCollapsed = childContainer.classList.toggle('collapsed');
          chevron.classList.toggle('expanded', !isCollapsed);
          icon.innerHTML = isCollapsed ? '&#128194;' : '&#128193;';
        });

        renderTreeNode(child, depth + 1, childContainer, pathPrefix ? pathPrefix + '/' + dirName : dirName);
    }

    function renderFileNode(file, depth, container, pathPrefix) {
        const btn = document.createElement('button');
        btn.className = 'tree-item';
        btn.type = 'button';
        btn.dataset.path = file.path;
        btn.style.paddingLeft = (depth * 16) + 'px';

        const chevronPlaceholder = document.createElement('span');
        chevronPlaceholder.className = 'tree-chevron placeholder';

        const icon = document.createElement('span');
        icon.className = 'tree-icon file-icon';
        const isPDF = isPdfPath(file.path);
        const isHTML = isHtmlPath(file.path);
        const isImage = isImagePath(file.path);
        const isLog = isLogPath(file.path);
        const isViewerOnly = isPDF || isHTML || isImage || isLog;
        if (isPDF) {
          icon.innerHTML = '&#128196;'; // page
          icon.style.color = '#e74c3c';
        } else if (isHTML) {
          icon.innerHTML = '&#127760;'; // globe
          icon.style.color = '#e67e22';
        } else if (isImage) {
          icon.innerHTML = '&#128247;'; // camera
          icon.style.color = '#3498db';
        } else if (isLog) {
          icon.innerHTML = '&#128220;'; // scroll
          icon.style.color = '#27ae60';
        } else {
          icon.innerHTML = '&#128462;';
        }

        const label = document.createElement('span');
        label.className = 'tree-label';
        label.textContent = file.name;
        label.title = file.path;

        btn.appendChild(chevronPlaceholder);
        btn.appendChild(icon);
        btn.appendChild(label);

        const fullFilePath = baseFolderPath ? baseFolderPath + '/' + file.path : file.path;

        // Show relative time when sorting by modified
        if (sortMode === 'modified') {
          const meta = fileMeta[fullFilePath];
          if (meta && meta.modifiedAt) {
            const timeSpan = document.createElement('span');
            timeSpan.className = 'tree-tag';
            timeSpan.style.color = 'var(--muted)';
            timeSpan.style.fontSize = '11px';
            timeSpan.textContent = relativeTime(meta.modifiedAt);
            timeSpan.title = new Date(meta.modifiedAt).toLocaleString();
            btn.appendChild(timeSpan);
          }
        }

        const tags = getEffectiveTags(fullFilePath);
        if (!isViewerOnly && tags.length > 0) {
          const tagSpan = document.createElement('span');
          tagSpan.className = 'tree-tag';
          tagSpan.textContent = tags.map(t => TAG_ICONS[t] || '').filter(Boolean).join('');
          tagSpan.title = tags.join(', ');
          btn.appendChild(tagSpan);
        }

        btn.addEventListener('click', () => {
          openFile(fullFilePath, true);
        });
        btn.addEventListener('contextmenu', (e) => {
          if (isViewerOnly) return;
          e.preventDefault();
          showTagMenu(e.clientX, e.clientY, fullFilePath);
        });
        container.appendChild(btn);
    }

    function renderFileList() {
      fileListEl.innerHTML = '';
      let displayFiles = files;
      if (baseFolderPath) {
        const prefix = baseFolderPath + '/';
        displayFiles = files.map(f => f.startsWith(prefix) ? f.slice(prefix.length) : f);
      }
      if (activeTagFilters.size > 0) {
        displayFiles = displayFiles.filter(f => {
          const fullPath = baseFolderPath ? baseFolderPath + '/' + f : f;
          return fileMatchesTagFilter(fullPath);
        });
      }
      const tree = buildTree(displayFiles);
      renderTreeNode(tree, 0, fileListEl, baseFolderPath || '');
      highlightActiveFile();
    }

    function highlightActiveFile() {
      for (const item of fileListEl.querySelectorAll('.tree-item[data-path]')) {
        let displayPath = item.dataset.path;
        if (baseFolderPath) displayPath = baseFolderPath + '/' + displayPath;
        const isActive = displayPath === activeFile;
        item.classList.toggle('active', isActive);
        if (isActive) expandAncestors(item);
      }
      for (const item of fileListEl.querySelectorAll('.search-result-item')) {
        const pathDiv = item.querySelector('.search-result-path');
        item.classList.toggle('active', pathDiv && pathDiv.textContent === activeFile);
      }
    }

    function expandAncestors(el) {
      let node = el.parentElement;
      while (node && node !== fileListEl) {
        if (node.classList.contains('tree-children') && node.classList.contains('collapsed')) {
          node.classList.remove('collapsed');
          const folderBtn = node.previousElementSibling;
          if (folderBtn) {
            const chevron = folderBtn.querySelector('.tree-chevron');
            const icon = folderBtn.querySelector('.tree-icon');
            if (chevron) chevron.classList.add('expanded');
            if (icon) icon.innerHTML = '&#128193;';
          }
        }
        node = node.parentElement;
      }
    }

    async function openFile(filePath, pushState, searchQuery) {
      try {
        stopLogTail();
        // Force exit raw/edit mode before rendering anything
        if (showingRaw) {
          showingRaw = false;
          renderedEl.classList.remove('hidden');
          rawContainerEl.classList.add('hidden');
          toggleRawBtn.textContent = 'Show Raw';
        }

        if (isLogPath(filePath)) {
          await openLogFile(filePath, pushState);
          return;
        }

        if (isPdfPath(filePath) || isHtmlPath(filePath) || isImagePath(filePath)) {
          activeFile = filePath;
          rawContent = '';
          fileNameEl.textContent = activeFile;
          document.title = filePath.split('/').pop() + ' - Markdown Viewer';

          const mediaURL = mediaUrlFor(filePath);
          let viewer;
          if (isImagePath(filePath)) {
            viewer =
              '<div style="display:flex;flex-direction:column;gap:8px;">' +
                '<div style="display:flex;gap:8px;">' +
                  '<a href="' + mediaURL + '" target="_blank" class="btn" style="text-decoration:none;">Open in new tab ↗</a>' +
                  '<a href="' + mediaURL + '" download class="btn" style="text-decoration:none;">Download ⬇</a>' +
                '</div>' +
                '<div style="text-align:center;"><img src="' + mediaURL + '" alt="' + activeFile + '" style="max-width:100%;height:auto;border:1px solid var(--border);border-radius:6px;" /></div>' +
              '</div>';
          } else {
            // PDF and HTML render in an iframe. HTML is sandboxed in an opaque
            // origin: scripts/relative assets still render for self-contained
            // pages, but the framed file cannot script the viewer or read its
            // origin storage.
            const sandboxAttr = isHtmlPath(filePath)
              ? ' sandbox="allow-scripts allow-popups allow-forms allow-modals"'
              : '';
            viewer =
              '<div style="display:flex;flex-direction:column;height:80vh;gap:8px;">' +
                '<div style="display:flex;gap:8px;">' +
                  '<a href="' + mediaURL + '" target="_blank" class="btn" style="text-decoration:none;">Open in new tab ↗</a>' +
                  '<a href="' + mediaURL + '" download class="btn" style="text-decoration:none;">Download ⬇</a>' +
                '</div>' +
                '<iframe src="' + mediaURL + '"' + sandboxAttr + ' style="flex:1;width:100%;border:1px solid var(--border);border-radius:6px;background:#fff;"></iframe>' +
              '</div>';
          }
          renderedEl.innerHTML = viewer;
          toggleRawBtn.classList.add('hidden');
          highlightActiveFile();
          updateNavButtons();
          renderHeaderTags();

          // Auto-close sidebar on mobile
          if (window.innerWidth <= 768 && !sidebarHidden) {
            applySidebarVisibility(true, false);
          }

          if (pushState) {
            const url = new URL(window.location.href);
            url.searchParams.set('file', activeFile);
            if (baseFolderPath) url.searchParams.set('baseFolderPath', baseFolderPath);
            url.searchParams.set('sidebar', sidebarHidden ? '0' : '1');
            url.searchParams.delete('fullscreen');
            window.history.pushState({ file: activeFile, sidebar: !sidebarHidden }, '', url);
          }
          return;
        }

        const response = await fetch('/api/file?path=' + encodeURIComponent(filePath));
        if (!response.ok) throw new Error('failed to load file');
        const payload = await response.json();

        activeFile = payload.path;
        rawContent = payload.content;
        fileNameEl.textContent = activeFile;
        rawCodeEl.textContent = rawContent;
        renderedEl.innerHTML = renderMarkdown(rawContent);
        rewriteLocalLinks(renderedEl);
        const firstHeader = renderedEl.querySelector('h1, h2, h3, h4, h5, h6');
        document.title = firstHeader ? firstHeader.textContent.trim() + ' - Markdown Viewer' : 'Markdown Viewer';
        await renderMermaid();
        highlightActiveFile();
        toggleRawBtn.classList.remove('hidden');
        updateNavButtons();
        renderHeaderTags();

        // Auto-close sidebar on mobile when a file is opened
        if (window.innerWidth <= 768 && !sidebarHidden) {
          applySidebarVisibility(true, false);
        }

        // Mark as opened if not already
        if (!fileOpened[activeFile]) {
          fileOpened[activeFile] = true;
          fetch('/api/opened', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ path: activeFile })
          }).catch(() => {});
          if (!searchMode) renderFileList();
        }

        // Clear previous highlights
        renderedEl.querySelectorAll('mark.search-highlight').forEach(m => {
          m.replaceWith(m.textContent);
        });

        if (searchQuery) {
          scrollToMatch(renderedEl, searchQuery);
        }

        if (pushState) {
          const url = new URL(window.location.href);
          url.searchParams.set('file', activeFile);
          if (baseFolderPath) url.searchParams.set('baseFolderPath', baseFolderPath);
          url.searchParams.set('sidebar', sidebarHidden ? '0' : '1');
          url.searchParams.delete('fullscreen');
          window.history.pushState({ file: activeFile, sidebar: !sidebarHidden }, '', url);
        }
      } catch (err) {
        renderedEl.innerHTML = '<div class="muted">Failed to load markdown file.</div>';
      }
    }

    // --- Log viewer (live tail, clear, JSON table, column config, views) ---
    let logState = null;

    const LOG_PREFERRED_COLS = ['time','timestamp','ts','date','level','lvl','severity','logger','name','msg','message'];
    const LOG_LEVEL_KEYS = new Set(['level','lvl','severity']);

    function logColDefaultWidth(key) {
      const k = key.toLowerCase();
      if (LOG_LEVEL_KEYS.has(k)) return 90;
      if (['time','timestamp','ts','date'].includes(k)) return 200;
      if (['msg','message'].includes(k)) return 360;
      return 160;
    }

    function stopLogTail() {
      if (logState && logState.timer) {
        clearInterval(logState.timer);
        logState.timer = null;
      }
      if (logState) logState.live = false;
    }

    function escapeHtml(s) {
      return String(s)
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function logConfigKey(path) { return 'mdviewer-logcfg:' + path; }

    function captureLogConfig() {
      const c = logState.colConfig;
      return {
        jsonMode: logState.jsonMode,
        globalFilter: logState.globalFilter,
        order: c.order.slice(),
        hidden: Object.keys(c.hidden).filter(k => c.hidden[k]),
        widths: Object.assign({}, c.widths),
        filters: Object.assign({}, c.filters)
      };
    }

    function applyLogConfig(cfg) {
      if (!cfg) return;
      logState.jsonMode = !!cfg.jsonMode;
      logState.globalFilter = cfg.globalFilter || '';
      const c = logState.colConfig;
      c.order = Array.isArray(cfg.order) ? cfg.order.slice() : [];
      c.hidden = {};
      (cfg.hidden || []).forEach(k => { c.hidden[k] = true; });
      c.widths = Object.assign({}, cfg.widths || {});
      c.filters = Object.assign({}, cfg.filters || {});
    }

    function saveLocalLogConfig() {
      if (!logState) return;
      try { localStorage.setItem(logConfigKey(logState.path), JSON.stringify(captureLogConfig())); } catch (e) {}
    }

    function loadLocalLogConfig() {
      try {
        const raw = localStorage.getItem(logConfigKey(logState.path));
        if (raw) applyLogConfig(JSON.parse(raw));
      } catch (e) {}
    }

    async function openLogFile(filePath, pushState) {
      stopLogTail();
      activeFile = filePath;
      rawContent = '';
      fileNameEl.textContent = activeFile;
      document.title = filePath.split('/').pop() + ' - Log Viewer';
      toggleRawBtn.classList.add('hidden');

      logState = {
        path: filePath, offset: -1, buffer: '', live: false, timer: null,
        jsonMode: false, globalFilter: '',
        allCols: [],
        colConfig: { order: [], hidden: {}, widths: {}, filters: {} },
        stick: true, resizing: false,
        views: [], currentView: ''
      };
      loadLocalLogConfig();

      renderedEl.innerHTML =
        '<div class="log-toolbar">' +
          '<button id="log-live-btn" class="btn" type="button" title="Continuously fetch new log lines">▶ Live Tail</button>' +
          '<button id="log-clear-btn" class="btn" type="button" title="Truncate this log file">🗑 Clear</button>' +
          '<label style="display:flex;align-items:center;gap:5px;font-size:12px;color:var(--muted);">' +
            '<input type="checkbox" id="log-json-toggle"> JSON table' +
          '</label>' +
          '<div class="log-dropdown"><button id="log-cols-btn" class="btn hidden" type="button">⚙ Columns</button>' +
            '<div id="log-cols-menu" class="log-menu hidden"></div></div>' +
          '<div class="log-dropdown"><button id="log-views-btn" class="btn" type="button">🔖 Views</button>' +
            '<div id="log-views-menu" class="log-menu hidden"></div></div>' +
          '<input type="text" id="log-filter" class="log-filter" placeholder="Filter all (substring)…">' +
          '<span id="log-view-name" class="log-stat"></span>' +
          '<span id="log-stat" class="log-stat"></span>' +
        '</div>' +
        '<div id="log-body"></div>';

      const liveBtn = document.getElementById('log-live-btn');
      const clearBtn = document.getElementById('log-clear-btn');
      const jsonToggle = document.getElementById('log-json-toggle');
      const filterInput = document.getElementById('log-filter');
      const colsBtn = document.getElementById('log-cols-btn');
      const viewsBtn = document.getElementById('log-views-btn');

      jsonToggle.checked = logState.jsonMode;
      filterInput.value = logState.globalFilter;
      if (logState.jsonMode) colsBtn.classList.remove('hidden');

      liveBtn.addEventListener('click', () => toggleLogLive());
      clearBtn.addEventListener('click', () => clearLog());
      jsonToggle.addEventListener('change', () => {
        logState.jsonMode = jsonToggle.checked;
        colsBtn.classList.toggle('hidden', !logState.jsonMode);
        saveLocalLogConfig(); renderLog();
      });
      filterInput.addEventListener('input', () => {
        logState.globalFilter = filterInput.value; saveLocalLogConfig(); renderLog();
      });
      colsBtn.addEventListener('click', (e) => { e.stopPropagation(); toggleLogMenu('log-cols-menu', renderColumnsMenu); });
      viewsBtn.addEventListener('click', (e) => { e.stopPropagation(); toggleLogMenu('log-views-menu', renderViewsMenu); });
      document.removeEventListener('click', closeLogMenus);
      document.addEventListener('click', closeLogMenus);

      highlightActiveFile();
      updateNavButtons();
      renderHeaderTags();
      if (window.innerWidth <= 768 && !sidebarHidden) applySidebarVisibility(true, false);

      if (pushState) {
        const url = new URL(window.location.href);
        url.searchParams.set('file', activeFile);
        if (baseFolderPath) url.searchParams.set('baseFolderPath', baseFolderPath);
        url.searchParams.set('sidebar', sidebarHidden ? '0' : '1');
        url.searchParams.delete('fullscreen');
        window.history.pushState({ file: activeFile, sidebar: !sidebarHidden }, '', url);
      }

      loadLogViews();
      await fetchLog(true);
    }

    function toggleLogMenu(menuId, renderFn) {
      const menu = document.getElementById(menuId);
      if (!menu) return;
      const wasHidden = menu.classList.contains('hidden');
      closeLogMenus();
      if (wasHidden) { renderFn(); menu.classList.remove('hidden'); }
    }
    function closeLogMenus() {
      document.querySelectorAll('.log-menu').forEach(m => m.classList.add('hidden'));
    }

    async function fetchLog(initial) {
      if (!logState) return;
      try {
        let url = '/api/log?path=' + encodeURIComponent(logState.path);
        if (!initial) url += '&offset=' + logState.offset;
        const resp = await fetch(url);
        if (!resp.ok) throw new Error('failed');
        const data = await resp.json();
        if (!logState || logState.path !== activeFile) return;

        if (initial || (data.truncated && data.offset <= logState.offset)) {
          logState.buffer = data.content;
        } else if (data.content) {
          logState.buffer += data.content;
        }
        const MAX_BUF = 5 << 20;
        if (logState.buffer.length > MAX_BUF) {
          const cut = logState.buffer.indexOf('\n', logState.buffer.length - MAX_BUF);
          logState.buffer = logState.buffer.slice(cut >= 0 ? cut + 1 : logState.buffer.length - MAX_BUF);
        }
        logState.offset = data.offset;
        if (!logState.resizing) renderLog();
      } catch (e) {
        const stat = document.getElementById('log-stat');
        if (stat) stat.textContent = 'fetch error';
      }
    }

    function toggleLogLive() {
      if (!logState) return;
      const liveBtn = document.getElementById('log-live-btn');
      if (logState.live) {
        stopLogTail();
        liveBtn.textContent = '▶ Live Tail';
        liveBtn.classList.remove('log-live-on');
      } else {
        logState.live = true;
        logState.stick = true;
        liveBtn.textContent = '⏸ Live (on)';
        liveBtn.classList.add('log-live-on');
        logState.timer = setInterval(() => fetchLog(false), 1500);
      }
    }

    async function clearLog() {
      if (!logState) return;
      if (!confirm('Clear (truncate) this log file? This cannot be undone.')) return;
      try {
        const resp = await fetch('/api/log/clear?path=' + encodeURIComponent(logState.path), { method: 'POST' });
        if (!resp.ok) throw new Error('failed');
        logState.buffer = '';
        logState.offset = 0;
        logState.stick = true;
        renderLog();
      } catch (e) {
        alert('Failed to clear log file.');
      }
    }

    function tryParseJson(line) {
      const t = line.trim();
      if (!t || (t[0] !== '{' && t[0] !== '[')) return null;
      try { const v = JSON.parse(t); return (v && typeof v === 'object') ? v : null; } catch (e) { return null; }
    }

    function logLines() {
      const lines = logState.buffer.split('\n');
      if (lines.length && lines[lines.length - 1] === '') lines.pop();
      return lines;
    }

    // Update logState.allCols with any newly-seen JSON keys, preserving order.
    function discoverColumns(parsedObjs) {
      const have = new Set(logState.allCols);
      if (logState.allCols.length === 0) {
        LOG_PREFERRED_COLS.forEach(k => {
          if (parsedObjs.some(o => k in o) && !have.has(k)) { logState.allCols.push(k); have.add(k); }
        });
      }
      parsedObjs.forEach(o => Object.keys(o).forEach(k => {
        if (!have.has(k)) { logState.allCols.push(k); have.add(k); }
      }));
    }

    function orderedColumns() {
      const c = logState.colConfig;
      const known = new Set(logState.allCols);
      const ordered = [];
      const placed = new Set();
      (c.order || []).forEach(k => { if (known.has(k) && !placed.has(k)) { ordered.push(k); placed.add(k); } });
      logState.allCols.forEach(k => { if (!placed.has(k)) { ordered.push(k); placed.add(k); } });
      return ordered;
    }
    function visibleColumns() {
      return orderedColumns().filter(k => !logState.colConfig.hidden[k]);
    }

    function renderLog() {
      if (!logState) return;
      const body = document.getElementById('log-body');
      if (!body) return;

      // Preserve scroll position & focused filter input across re-render.
      const prevScroller = body.querySelector('.log-output, .log-table-wrap');
      const prevTop = prevScroller ? prevScroller.scrollTop : 0;
      const active = document.activeElement;
      let focusInfo = null;
      if (active && active.classList && active.classList.contains('log-col-filter')) {
        focusInfo = { col: active.dataset.col, start: active.selectionStart, end: active.selectionEnd };
      }

      if (logState.jsonMode) {
        renderLogTable(body);
      } else {
        renderLogRaw(body);
      }

      const scroller = body.querySelector('.log-output, .log-table-wrap');
      if (scroller) {
        if (logState.stick) {
          scroller.scrollTop = scroller.scrollHeight;
        } else {
          scroller.scrollTop = prevTop;
        }
        scroller.addEventListener('scroll', onLogScroll);
      }
      if (focusInfo) {
        const inp = body.querySelector('.log-col-filter[data-col="' + cssAttr(focusInfo.col) + '"]');
        if (inp) { inp.focus(); try { inp.setSelectionRange(focusInfo.start, focusInfo.end); } catch (e) {} }
      }
    }

    function cssAttr(s) { return String(s).replace(/"/g, '\\"'); }

    function onLogScroll(e) {
      const el = e.currentTarget;
      logState.stick = (el.scrollTop + el.clientHeight >= el.scrollHeight - 40);
    }

    function updateLogStat(shownCount, total) {
      const stat = document.getElementById('log-stat');
      if (stat) stat.textContent = shownCount + (shownCount !== total ? ' / ' + total : '') + ' lines';
    }

    function highlightFilter(text, filter) {
      const esc = escapeHtml(text);
      if (!filter) return esc;
      const idx = text.toLowerCase().indexOf(filter);
      if (idx < 0) return esc;
      const end = idx + filter.length;
      return escapeHtml(text.slice(0, idx)) +
        '<mark class="search-highlight">' + escapeHtml(text.slice(idx, end)) + '</mark>' +
        escapeHtml(text.slice(end));
    }

    function renderLogRaw(body) {
      const filter = logState.globalFilter.trim().toLowerCase();
      const all = logLines();
      const shown = filter ? all.filter(l => l.toLowerCase().includes(filter)) : all;
      updateLogStat(shown.length, all.length);
      if (!shown.length) {
        body.innerHTML = '<div class="muted" style="padding:12px;">No log lines' + (filter ? ' match the filter.' : '.') + '</div>';
        return;
      }
      const html = shown.map(l =>
        '<div class="log-line' + (filter ? ' log-hit' : '') + '">' + highlightFilter(l, filter) + '</div>'
      ).join('');
      body.innerHTML = '<pre class="log-output">' + html + '</pre>';
    }

    function levelBadge(val) {
      const lvl = String(val).toLowerCase().trim();
      const known = ['error','fatal','critical','warn','warning','info','debug','trace'];
      if (known.includes(lvl)) {
        return '<span class="log-badge log-lvl-' + lvl + '">' + escapeHtml(String(val)) + '</span>';
      }
      return escapeHtml(String(val));
    }

    function cellString(v) {
      if (v === null || v === undefined) return '';
      if (typeof v === 'object') return JSON.stringify(v);
      return String(v);
    }

    function renderLogTable(body) {
      const globalFilter = logState.globalFilter.trim().toLowerCase();
      const all = logLines();
      const rows = all.map(l => ({ raw: l, obj: tryParseJson(l) }));
      const parsedObjs = rows.filter(r => r.obj).map(r => r.obj);
      if (!parsedObjs.length) { renderLogRaw(body); return; }

      discoverColumns(parsedObjs);
      const cols = visibleColumns();
      const c = logState.colConfig;

      const colFilters = Object.keys(c.filters).filter(k => (c.filters[k] || '').trim() !== '');
      const anyColFilter = colFilters.length > 0;

      const shown = rows.filter(r => {
        if (globalFilter && !r.raw.toLowerCase().includes(globalFilter)) return false;
        if (!anyColFilter) return true;
        if (!r.obj) return false; // column filters exclude non-JSON lines
        return colFilters.every(k => cellString(r.obj[k]).toLowerCase().includes(c.filters[k].trim().toLowerCase()));
      });
      updateLogStat(shown.length, all.length);

      let html = '<div class="log-table-wrap"><table class="log-table"><colgroup>';
      cols.forEach(k => {
        const w = c.widths[k] || logColDefaultWidth(k);
        html += '<col style="width:' + w + 'px">';
      });
      html += '</colgroup><thead><tr>';
      cols.forEach(k => {
        html += '<th data-col="' + escapeHtml(k) + '"><span class="log-th-label" title="' + escapeHtml(k) + '">' +
          escapeHtml(k) + '</span><span class="log-col-resizer" data-col="' + escapeHtml(k) + '"></span></th>';
      });
      html += '</tr><tr class="log-filter-row">';
      cols.forEach(k => {
        const val = c.filters[k] || '';
        html += '<th><input class="log-col-filter" data-col="' + escapeHtml(k) +
          '" type="text" placeholder="filter…" value="' + escapeHtml(val) + '"></th>';
      });
      html += '</tr></thead><tbody>';

      shown.forEach(r => {
        if (!r.obj) {
          html += '<tr class="log-raw-row"><td colspan="' + cols.length + '">' + highlightFilter(r.raw, globalFilter) + '</td></tr>';
          return;
        }
        html += '<tr>';
        cols.forEach(k => {
          if (!(k in r.obj)) { html += '<td></td>'; return; }
          const v = cellString(r.obj[k]);
          const hl = (c.filters[k] || '').trim().toLowerCase() || globalFilter;
          if (LOG_LEVEL_KEYS.has(k.toLowerCase())) html += '<td>' + levelBadge(v) + '</td>';
          else html += '<td title="' + escapeHtml(v) + '">' + highlightFilter(v, hl) + '</td>';
        });
        html += '</tr>';
      });
      html += '</tbody></table></div>';
      body.innerHTML = html;

      // Wire column filters and resizers.
      body.querySelectorAll('.log-col-filter').forEach(inp => {
        inp.addEventListener('input', () => {
          logState.colConfig.filters[inp.dataset.col] = inp.value;
          saveLocalLogConfig(); renderLog();
        });
      });
      body.querySelectorAll('.log-col-resizer').forEach(rz => {
        rz.addEventListener('mousedown', startColResize);
      });
    }

    function startColResize(e) {
      e.preventDefault();
      e.stopPropagation();
      const col = e.currentTarget.dataset.col;
      const th = e.currentTarget.closest('th');
      const startX = e.clientX;
      const startW = th.getBoundingClientRect().width;
      logState.resizing = true;
      const colEl = th.closest('table').querySelector('colgroup col:nth-child(' +
        (Array.prototype.indexOf.call(th.parentNode.children, th) + 1) + ')');
      function onMove(ev) {
        const w = Math.max(48, Math.round(startW + (ev.clientX - startX)));
        logState.colConfig.widths[col] = w;
        if (colEl) colEl.style.width = w + 'px';
      }
      function onUp() {
        document.removeEventListener('mousemove', onMove);
        document.removeEventListener('mouseup', onUp);
        logState.resizing = false;
        saveLocalLogConfig();
      }
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    }

    function renderColumnsMenu() {
      const menu = document.getElementById('log-cols-menu');
      const cols = orderedColumns();
      if (!cols.length) { menu.innerHTML = '<div class="log-menu-empty">Enable JSON table with data to configure columns.</div>'; return; }
      let html = '<div class="log-menu-title">Visible columns</div>';
      cols.forEach(k => {
        const checked = logState.colConfig.hidden[k] ? '' : 'checked';
        html += '<label class="log-menu-row"><input type="checkbox" data-col="' + escapeHtml(k) + '" ' + checked + '> ' + escapeHtml(k) + '</label>';
      });
      html += '<div class="log-menu-actions"><button class="btn" id="log-cols-reset" type="button">Reset</button></div>';
      menu.innerHTML = html;
      menu.querySelectorAll('input[type="checkbox"]').forEach(cb => {
        cb.addEventListener('change', () => {
          logState.colConfig.hidden[cb.dataset.col] = !cb.checked;
          saveLocalLogConfig(); renderLog();
        });
      });
      menu.querySelector('#log-cols-reset').addEventListener('click', () => {
        logState.colConfig.hidden = {};
        logState.colConfig.widths = {};
        saveLocalLogConfig(); renderColumnsMenu(); renderLog();
      });
    }

    async function loadLogViews() {
      try {
        const resp = await fetch('/api/log/views?path=' + encodeURIComponent(logState.path));
        if (!resp.ok) return;
        const data = await resp.json();
        if (!logState) return;
        logState.views = data.views || [];
      } catch (e) {}
    }

    function renderViewsMenu() {
      const menu = document.getElementById('log-views-menu');
      let html = '<div class="log-menu-title">Saved views</div>';
      if (!logState.views.length) {
        html += '<div class="log-menu-empty">No views yet.</div>';
      } else {
        logState.views.forEach(v => {
          const active = v.name === logState.currentView ? ' log-view-active' : '';
          const scope = v.dir ? v.dir + '/' : 'root';
          html += '<div class="log-menu-row log-view-row' + active + '">' +
            '<span class="log-view-apply" data-name="' + escapeHtml(v.name) + '">' + escapeHtml(v.name) +
            ' <span class="log-view-scope">' + escapeHtml(scope) + '</span></span>' +
            '<span class="log-view-del" data-name="' + escapeHtml(v.name) + '" title="Delete view">×</span></div>';
        });
      }
      html += '<div class="log-menu-actions">' +
        '<button class="btn" id="log-view-save" type="button">💾 Save current as view…</button>' +
        (logState.currentView ? '<button class="btn" id="log-view-clear" type="button">Clear</button>' : '') +
        '</div>';
      menu.innerHTML = html;

      menu.querySelectorAll('.log-view-apply').forEach(el => {
        el.addEventListener('click', () => applyLogView(el.dataset.name));
      });
      menu.querySelectorAll('.log-view-del').forEach(el => {
        el.addEventListener('click', (e) => { e.stopPropagation(); deleteLogView(el.dataset.name); });
      });
      menu.querySelector('#log-view-save').addEventListener('click', saveLogView);
      const clr = menu.querySelector('#log-view-clear');
      if (clr) clr.addEventListener('click', () => {
        logState.currentView = '';
        updateViewNameLabel();
        renderViewsMenu();
      });
    }

    function updateViewNameLabel() {
      const el = document.getElementById('log-view-name');
      if (el) el.textContent = logState.currentView ? ('view: ' + logState.currentView) : '';
    }

    function applyLogView(name) {
      const v = logState.views.find(x => x.name === name);
      if (!v) return;
      applyLogConfig(v.config || {});
      logState.currentView = name;
      const jsonToggle = document.getElementById('log-json-toggle');
      const filterInput = document.getElementById('log-filter');
      const colsBtn = document.getElementById('log-cols-btn');
      if (jsonToggle) jsonToggle.checked = logState.jsonMode;
      if (filterInput) filterInput.value = logState.globalFilter;
      if (colsBtn) colsBtn.classList.toggle('hidden', !logState.jsonMode);
      saveLocalLogConfig();
      updateViewNameLabel();
      closeLogMenus();
      renderLog();
    }

    async function saveLogView() {
      const name = prompt('Save view as (available to this folder and subfolders):', logState.currentView || '');
      if (!name || !name.trim()) return;
      try {
        const resp = await fetch('/api/log/views/save?path=' + encodeURIComponent(logState.path), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: name.trim(), config: captureLogConfig() })
        });
        if (!resp.ok) throw new Error('failed');
        logState.currentView = name.trim();
        updateViewNameLabel();
        await loadLogViews();
        renderViewsMenu();
      } catch (e) {
        alert('Failed to save view.');
      }
    }

    async function deleteLogView(name) {
      if (!confirm('Delete view "' + name + '"?')) return;
      try {
        const resp = await fetch('/api/log/views/delete?path=' + encodeURIComponent(logState.path), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: name })
        });
        if (!resp.ok) throw new Error('failed');
        if (logState.currentView === name) { logState.currentView = ''; updateViewNameLabel(); }
        await loadLogViews();
        renderViewsMenu();
      } catch (e) {
        alert('Failed to delete view.');
      }
    }

    function renderMarkdown(markdown) {
      // Custom renderer to rewrite relative image paths
      const renderer = new marked.Renderer();
      const origImage = renderer.image.bind(renderer);
      renderer.image = function({ href, title, text }) {
        if (href && !href.match(/^https?:\/\//) && !href.startsWith('data:') && !href.startsWith('/')) {
          // Resolve relative to the current file's directory
          const dir = activeFile ? activeFile.substring(0, activeFile.lastIndexOf('/')) : '';
          const resolved = dir ? dir + '/' + href : href;
          href = '/api/media/' + resolved;
        }
        const titleAttr = title ? ' title="' + title + '"' : '';
        return '<img src="' + href + '" alt="' + (text || '') + '"' + titleAttr + ' />';
      };

      // Generate heading IDs for TOC anchor links
      const origHeading = renderer.heading.bind(renderer);
      renderer.heading = function({ text, depth }) {
        // Strip HTML tags to get raw text for slug
        const raw = text.replace(/<[^>]*>/g, '');
        const slug = raw.toLowerCase().replace(/[^\w\s-]/g, '').replace(/\s+/g, '-').replace(/-+/g, '-').replace(/^-|-$/g, '');
        return '<h' + depth + ' id="' + slug + '"><a class="anchor" href="#' + slug + '"></a>' + text + '</h' + depth + '>\n';
      };

      let html = marked.parse(markdown, { gfm: true, renderer: renderer });
      html = html.replace(/<pre><code class="language-mermaid">([\s\S]*?)<\/code><\/pre>/g, (_, code) => {
        return '<div class="mermaid-block"><div class="mermaid">' + decodeHTML(code) + '</div></div>';
      });
      return html;
    }

    // resolveLocalLink resolves a relative/root-relative href against the
    // active file's directory. Returns { path, hash } or null for external,
    // in-page anchor, or out-of-root links.
    function resolveLocalLink(href) {
      if (!href) return null;
      href = href.trim();
      if (href === '' || href.startsWith('#') || href.startsWith('//')) return null;
      if (/^[a-z][a-z0-9+.\-]*:/i.test(href)) return null; // http:, mailto:, data:, etc.
      let hash = '';
      const hashIdx = href.indexOf('#');
      if (hashIdx >= 0) { hash = href.substring(hashIdx); href = href.substring(0, hashIdx); }
      const queryIdx = href.indexOf('?');
      if (queryIdx >= 0) href = href.substring(0, queryIdx);
      if (href === '') return null;
      if (href.startsWith('/api/')) return null;
      let decoded;
      try { decoded = decodeURIComponent(href); } catch (e) { decoded = href; }
      const parts = [];
      if (!decoded.startsWith('/') && activeFile && activeFile.includes('/')) {
        parts.push(...activeFile.substring(0, activeFile.lastIndexOf('/')).split('/'));
      }
      for (const seg of decoded.split('/')) {
        if (seg === '' || seg === '.') continue;
        if (seg === '..') {
          if (!parts.length) return null;
          parts.pop();
          continue;
        }
        parts.push(seg);
      }
      if (!parts.length) return null;
      return { path: parts.join('/'), hash: hash };
    }

    function viewerUrlFor(filePath, hash) {
      const params = new URLSearchParams();
      params.set('file', filePath);
      if (baseFolderPath) params.set('baseFolderPath', baseFolderPath);
      params.set('sidebar', sidebarHidden ? '0' : '1');
      return '/?' + params.toString() + (hash || '');
    }

    function isViewablePath(p) {
      return isMarkdownPath(p) || isPdfPath(p) || isHtmlPath(p) || isImagePath(p) || isLogPath(p);
    }

    // rewriteLocalLinks points relative file links in rendered markdown at
    // the viewer (/?file=...) instead of raw server paths.
    function rewriteLocalLinks(container) {
      container.querySelectorAll('a[href]').forEach(a => {
        if (a.classList.contains('anchor')) return;
        const target = resolveLocalLink(a.getAttribute('href'));
        if (!target) return;
        if (isViewablePath(target.path)) {
          a.setAttribute('href', viewerUrlFor(target.path, target.hash));
          a.dataset.file = target.path;
          if (target.hash) a.dataset.hash = target.hash;
        } else {
          a.setAttribute('href', mediaUrlFor(target.path));
        }
      });
    }

    renderedEl.addEventListener('click', async (e) => {
      if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      const a = e.target.closest('a[data-file]');
      if (!a || !renderedEl.contains(a) || (a.target && a.target !== '_self')) return;
      e.preventDefault();
      await openFile(a.dataset.file, true);
      const hash = a.dataset.hash;
      if (hash) {
        let id;
        try { id = decodeURIComponent(hash.substring(1)); } catch (err) { id = hash.substring(1); }
        const el = document.getElementById(id);
        if (el) el.scrollIntoView();
      }
    });

    async function renderMermaid() {
      if (!window.mermaid) return;
      const nodes = document.querySelectorAll('.mermaid');
      if (!nodes.length) return;
      try {
        await window.mermaid.run({ nodes });
      } catch (err) {
        console.error('mermaid render failed', err);
      }
    }

    function loadThemePreference() {
      const storedTheme = window.localStorage.getItem(STORAGE_THEME_KEY);
      applyTheme(storedTheme === 'dark' ? 'dark' : 'light');
    }

    function applyTheme(theme) {
      const resolvedTheme = theme === 'light' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', resolvedTheme);
      themeToggleBtn.textContent = resolvedTheme === 'dark' ? 'Light Mode' : 'Dark Mode';
    }

    function shouldHideSidebar(params) {
      // Explicit sidebar param takes highest precedence
      const sidebar = (params.get('sidebar') || '').toLowerCase();
      if (sidebar === '1' || sidebar === 'true' || sidebar === 'yes') {
        return false;
      }
      if (sidebar === '0' || sidebar === 'false' || sidebar === 'hidden') {
        return true;
      }

      // Legacy fullscreen param
      const fullscreen = (params.get('fullscreen') || '').toLowerCase();
      if (fullscreen === '1' || fullscreen === 'true' || fullscreen === 'yes') {
        return true;
      }

      // Hide sidebar by default when a file is specified in URL (and no explicit sidebar param)
      if (params.get('file')) {
        return true;
      }

      return false;
    }

    // Legacy alias
    function isFullscreenMode(params) { return shouldHideSidebar(params); }

    function applySidebarVisibility(hidden, pushState) {
      sidebarHidden = Boolean(hidden);
      appEl.classList.toggle('sidebar-hidden', sidebarHidden);
      toggleSidebarBtn.textContent = sidebarHidden ? 'Show Sidebar' : 'Hide Sidebar';

      if (!pushState) {
        return;
      }

      const url = new URL(window.location.href);
      if (activeFile) {
        url.searchParams.set('file', activeFile);
      }
      if (baseFolderPath) url.searchParams.set('baseFolderPath', baseFolderPath);
      // Use sidebar param; remove legacy fullscreen
      url.searchParams.set('sidebar', sidebarHidden ? '0' : '1');
      url.searchParams.delete('fullscreen');
      window.history.pushState({ file: activeFile, sidebar: !sidebarHidden }, '', url);
    }

    function decodeHTML(text) {
      const textarea = document.createElement('textarea');
      textarea.innerHTML = text;
      return textarea.value;
    }

    async function performSearch(query) {
      try {
        const response = await fetch('/api/search?q=' + encodeURIComponent(query));
        if (!response.ok) throw new Error('search failed');
        const payload = await response.json();
        searchMode = true;
        renderSearchResults(payload.results, payload.query);
      } catch (err) {
        fileListEl.innerHTML = '<div class="muted">Search failed.</div>';
      }
    }

    function renderSearchResults(results, query) {
      fileListEl.innerHTML = '';

      const info = document.createElement('div');
      info.className = 'search-info';
      info.textContent = results.length + ' result' + (results.length !== 1 ? 's' : '') + ' found';
      fileListEl.appendChild(info);

      if (results.length === 0) return;

      for (const r of results) {
        const btn = document.createElement('button');
        btn.className = 'search-result-item';
        btn.type = 'button';
        if (r.path === activeFile) btn.classList.add('active');

        const pathDiv = document.createElement('div');
        pathDiv.className = 'search-result-path';
        pathDiv.textContent = r.path;

        const ctxDiv = document.createElement('div');
        ctxDiv.className = 'search-result-context';
        ctxDiv.innerHTML = highlightQuery(escapeHtml(r.context), query);

        btn.appendChild(pathDiv);
        btn.appendChild(ctxDiv);
        btn.addEventListener('click', () => openFile(r.path, true, query));
        fileListEl.appendChild(btn);
      }
    }

    function highlightQuery(text, query) {
      const escaped = query.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      const re = new RegExp('(' + escaped + ')', 'gi');
      return text.replace(re, '<mark>$1</mark>');
    }

    function escapeHtml(str) {
      const div = document.createElement('div');
      div.textContent = str;
      return div.innerHTML;
    }

    function scrollToMatch(container, query) {
      const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT, null);
      const lowerQuery = query.toLowerCase();
      let node;
      while ((node = walker.nextNode())) {
        const idx = node.textContent.toLowerCase().indexOf(lowerQuery);
        if (idx < 0) continue;

        const range = document.createRange();
        range.setStart(node, idx);
        range.setEnd(node, idx + query.length);

        const mark = document.createElement('mark');
        mark.className = 'search-highlight';
        range.surroundContents(mark);

        mark.scrollIntoView({ behavior: 'smooth', block: 'center' });
        return;
      }
    }

    function showTagMenu(x, y, filePath) {
      closeTagMenu();
      const menu = document.createElement('div');
      menu.className = 'tag-menu';
      menu.id = 'tag-context-menu';

      const currentTags = fileTags[filePath] || [];

      for (const tag of TAG_LIST) {
        const item = document.createElement('button');
        item.className = 'tag-menu-item';
        item.type = 'button';
        const hasTag = currentTags.includes(tag);
        const prefix = hasTag ? '☑ ' : '☐ ';
        item.textContent = prefix + TAG_ICONS[tag] + ' ' + tag;
        item.addEventListener('click', (e) => {
          e.stopPropagation();
          if (hasTag) {
            setTag(filePath, tag, 'remove');
          } else {
            setTag(filePath, tag, 'add');
          }
          closeTagMenu();
        });
        menu.appendChild(item);
      }

      if (currentTags.length > 0) {
        const divider = document.createElement('div');
        divider.className = 'tag-menu-divider';
        menu.appendChild(divider);

        const clearItem = document.createElement('button');
        clearItem.className = 'tag-menu-item';
        clearItem.type = 'button';
        clearItem.textContent = '  ✕ Remove all tags';
        clearItem.addEventListener('click', () => { closeTagMenu(); setTag(filePath, '', 'clear'); });
        menu.appendChild(clearItem);
      }

      menu.style.left = x + 'px';
      menu.style.top = y + 'px';
      document.body.appendChild(menu);

      const rect = menu.getBoundingClientRect();
      if (rect.right > window.innerWidth) menu.style.left = (window.innerWidth - rect.width - 8) + 'px';
      if (rect.bottom > window.innerHeight) menu.style.top = (window.innerHeight - rect.height - 8) + 'px';

      setTimeout(() => document.addEventListener('click', closeTagMenu, { once: true }), 0);
    }

    function closeTagMenu() {
      const existing = document.getElementById('tag-context-menu');
      if (existing) existing.remove();
    }

    async function setTag(filePath, tag, action) {
      try {
        const resp = await fetch('/api/tag', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: filePath, tag: tag, action: action || 'add' })
        });
        if (!resp.ok) throw new Error('failed to set tag');

        // Update local state
        if (action === 'clear') {
          delete fileTags[filePath];
        } else if (action === 'remove') {
          const arr = fileTags[filePath] || [];
          fileTags[filePath] = arr.filter(t => t !== tag);
          if (fileTags[filePath].length === 0) delete fileTags[filePath];
        } else {
          const arr = fileTags[filePath] || [];
          if (!arr.includes(tag)) {
            fileTags[filePath] = [...arr, tag];
          }
        }
        if (!searchMode) renderFileList();
        if (filePath === activeFile) renderHeaderTags();
      } catch (err) {
        console.error('Failed to set tag:', err);
      }
    }

    function renderHeaderTags() {
      headerTagsEl.innerHTML = '';
      if (!activeFile) {
        headerTagsEl.classList.add('hidden');
        return;
      }
      headerTagsEl.classList.remove('hidden');
      const currentTags = fileTags[activeFile] || [];
      for (const tag of TAG_LIST) {
        const btn = document.createElement('button');
        btn.className = 'header-tag-btn';
        btn.type = 'button';
        if (currentTags.includes(tag)) btn.classList.add('active');
        btn.textContent = TAG_ICONS[tag] + ' ' + tag;
        btn.addEventListener('click', () => {
          if (currentTags.includes(tag)) {
            setTag(activeFile, tag, 'remove');
          } else {
            setTag(activeFile, tag, 'add');
          }
        });
        headerTagsEl.appendChild(btn);
      }
    }
    // ---- Edit Mode ----
    const editBtn = document.getElementById('edit-btn');
    const saveBtn = document.getElementById('save-btn');
    const cancelEditBtn = document.getElementById('cancel-edit-btn');
    let editMode = false;
    let editTextarea = null;

    editBtn.addEventListener('click', enterEditMode);
    saveBtn.addEventListener('click', saveEdit);
    cancelEditBtn.addEventListener('click', exitEditMode);

    function enterEditMode() {
      if (!activeFile || editMode) return;
      editMode = true;
      editBtn.classList.add('hidden');
      saveBtn.classList.remove('hidden');
      cancelEditBtn.classList.remove('hidden');
      toggleRawBtn.classList.add('hidden');
      rawContainerEl.classList.add('hidden');

      editTextarea = document.createElement('textarea');
      editTextarea.id = 'edit-textarea';
      editTextarea.value = rawContent;
      // Auto-resize
      editTextarea.addEventListener('input', () => {
        editTextarea.style.height = 'auto';
        editTextarea.style.height = editTextarea.scrollHeight + 'px';
      });
      // Tab support
      editTextarea.addEventListener('keydown', (e) => {
        if (e.key === 'Tab') {
          e.preventDefault();
          const start = editTextarea.selectionStart;
          const end = editTextarea.selectionEnd;
          editTextarea.value = editTextarea.value.substring(0, start) + '  ' + editTextarea.value.substring(end);
          editTextarea.selectionStart = editTextarea.selectionEnd = start + 2;
        }
        // Ctrl+S / Cmd+S to save
        if ((e.ctrlKey || e.metaKey) && e.key === 's') {
          e.preventDefault();
          saveEdit();
        }
      });

      renderedEl.classList.add('hidden');
      const viewer = document.querySelector('.viewer');
      viewer.appendChild(editTextarea);
      setTimeout(() => {
        editTextarea.style.height = editTextarea.scrollHeight + 'px';
        editTextarea.focus();
      }, 0);
    }

    async function saveEdit() {
      if (!editMode || !activeFile) return;
      const content = editTextarea ? editTextarea.value : rawContent;
      saveBtn.disabled = true;
      saveBtn.textContent = 'Saving…';
      try {
        const resp = await fetch('/api/save', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: activeFile, content: content })
        });
        if (!resp.ok) {
          const t = await resp.text();
          throw new Error(t);
        }
        rawContent = content;
        exitEditMode();
        // Re-render
        rawCodeEl.textContent = rawContent;
        renderedEl.innerHTML = renderMarkdown(rawContent);
        rewriteLocalLinks(renderedEl);
        await renderMermaid();
        attachCheckboxHandlers();
      } catch (err) {
        alert('Save failed: ' + err.message);
      } finally {
        saveBtn.disabled = false;
        saveBtn.textContent = '💾 Save';
      }
    }

    function exitEditMode() {
      editMode = false;
      editBtn.classList.remove('hidden');
      saveBtn.classList.add('hidden');
      cancelEditBtn.classList.add('hidden');
      toggleRawBtn.classList.remove('hidden');
      renderedEl.classList.remove('hidden');
      if (editTextarea) {
        editTextarea.remove();
        editTextarea = null;
      }
    }

    // ---- Checkbox Toggle ----
    function attachCheckboxHandlers() {
      const checkboxes = renderedEl.querySelectorAll('input[type="checkbox"]');
      checkboxes.forEach((cb, idx) => {
        cb.removeAttribute('disabled');
        cb.addEventListener('change', () => toggleCheckbox(idx, cb.checked));
      });
    }

    async function toggleCheckbox(index, checked) {
      // Find the nth checkbox pattern in raw markdown and toggle it
      let count = 0;
      const lines = rawContent.split('\n');
      for (let i = 0; i < lines.length; i++) {
        const match = lines[i].match(/^(\s*(?:[-*+]|\d+[.)]) \s*)\[([ xX])\]/);
        if (match) {
          if (count === index) {
            const prefix = match[1];
            const rest = lines[i].substring(match[0].length);
            lines[i] = prefix + '[' + (checked ? 'x' : ' ') + ']' + rest;
            break;
          }
          count++;
        }
      }
      const newContent = lines.join('\n');
      try {
        const resp = await fetch('/api/save', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ path: activeFile, content: newContent })
        });
        if (!resp.ok) throw new Error('save failed');
        rawContent = newContent;
        rawCodeEl.textContent = rawContent;
      } catch (err) {
        // Revert checkbox visually
        const checkboxes = renderedEl.querySelectorAll('input[type="checkbox"]');
        if (checkboxes[index]) checkboxes[index].checked = !checked;
      }
    }

    // Patch openFile to attach checkbox handlers after render
    const _origOpenFile = openFile;
    openFile = async function(filePath, pushState, searchQuery) {
      await _origOpenFile(filePath, pushState, searchQuery);
      const isMarkdown = isMarkdownPath(filePath);
      attachCheckboxHandlers();
      if (isMarkdown) {
        editBtn.classList.remove('hidden');
      } else {
        editBtn.classList.add('hidden');
      }
      if (editMode) exitEditMode();
      checkPodcastStatus(filePath);
    };

    // Trigger podcast check for the initially loaded file
    if (activeFile) checkPodcastStatus(activeFile);

    // ---- Podcast ----
    const podcastBtn = document.getElementById('podcast-btn');
    const podcastStatus = document.getElementById('podcast-status');
    const podcastPlayer = document.getElementById('podcast-player');
    let podcastPollTimer = null;

    async function checkPodcastStatus(filePath) {
      if (!filePath || !filePath.endsWith('.md')) {
        podcastBtn.classList.add('hidden');
        podcastPlayer.classList.add('hidden');
        podcastStatus.classList.add('hidden');
        return;
      }
      podcastBtn.classList.remove('hidden');

      try {
        const resp = await fetch('/api/podcast?path=' + encodeURIComponent(filePath));
        const data = await resp.json();

        if (data.status === 'done' && data.output) {
          podcastBtn.textContent = '🎙️ Regenerate';
          podcastPlayer.classList.remove('hidden');
          podcastPlayer.src = '/api/media/' + data.output;
          podcastStatus.classList.add('hidden');
          stopPodcastPoll();
        } else if (data.status === 'generating') {
          podcastBtn.textContent = '🎙️ Generating...';
          podcastBtn.disabled = true;
          podcastStatus.textContent = data.progress + '%';
          podcastStatus.classList.remove('hidden');
          podcastPlayer.classList.add('hidden');
          startPodcastPoll(filePath);
        } else if (data.status === 'error') {
          podcastBtn.textContent = '🎙️ Podcast';
          podcastBtn.disabled = false;
          podcastPlayer.classList.add('hidden');
          podcastStatus.classList.add('hidden');
          stopPodcastPoll();
          alert('Podcast generation failed:\n\n' + (data.error || 'Unknown error'));
        } else {
          podcastBtn.textContent = '🎙️ Podcast';
          podcastBtn.disabled = false;
          podcastPlayer.classList.add('hidden');
          podcastStatus.classList.add('hidden');
          stopPodcastPoll();
        }
      } catch (e) {
        podcastBtn.textContent = '🎙️ Podcast';
        podcastBtn.disabled = false;
      }
    }

    function startPodcastPoll(filePath) {
      stopPodcastPoll();
      podcastPollTimer = setInterval(() => checkPodcastStatus(filePath), 3000);
    }

    function stopPodcastPoll() {
      if (podcastPollTimer) {
        clearInterval(podcastPollTimer);
        podcastPollTimer = null;
      }
    }

    podcastBtn.addEventListener('click', async () => {
      if (!activeFile) return;

      // If podcast exists, confirm regeneration
      if (podcastBtn.textContent.includes('Regenerate')) {
        if (!confirm('Regenerate podcast? This will replace the existing one.')) return;
        await fetch('/api/podcast?path=' + encodeURIComponent(activeFile), { method: 'DELETE' });
      }

      podcastBtn.textContent = '🎙️ Starting...';
      podcastBtn.disabled = true;
      podcastStatus.textContent = '0%';
      podcastStatus.classList.remove('hidden');
      podcastPlayer.classList.add('hidden');

      try {
        await fetch('/api/podcast?path=' + encodeURIComponent(activeFile), { method: 'POST' });
        startPodcastPoll(activeFile);
      } catch (e) {
        podcastBtn.textContent = '🎙️ Podcast';
        podcastBtn.disabled = false;
        podcastStatus.textContent = 'Error: ' + e.message;
      }
    });

  </script>
</body>
</html>
`

const podcastsHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1,user-scalable=no"/>
<title>Podcasts – Markdown Viewer</title>
<style>
:root{--bg:#0d1117;--panel:#161b22;--border:#30363d;--text:#c9d1d9;--muted:#8b949e;--link:#58a6ff;--active:#1f6feb33;--sidebar-bg:#010409;--button-bg:#21262d;--button-hover:#30363d;--card-bg:#161b22;--card-border:#21262d;--card-shadow:0 1px 3px rgba(0,0,0,0.3);--success:#3fb950;--warn:#d29922}
@media(prefers-color-scheme:light){:root{--bg:#f6f8fa;--panel:#ffffff;--border:#d0d7de;--text:#1f2328;--muted:#656d76;--link:#0969da;--active:#0969da1a;--sidebar-bg:#f0f0f0;--button-bg:#e8e8e8;--button-hover:#d0d7de;--card-bg:#ffffff;--card-border:#d0d7de;--card-shadow:0 1px 3px rgba(0,0,0,0.08);--success:#1a7f37;--warn:#9a6700}}
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Helvetica,Arial,sans-serif;background:var(--bg);color:var(--text);min-height:100vh;padding-bottom:160px}
.header{padding:16px;display:flex;align-items:center;gap:12px;border-bottom:1px solid var(--border);background:var(--panel);position:sticky;top:0;z-index:10}
.header h1{font-size:1.2em;flex:1}
.header a{color:var(--link);text-decoration:none;font-size:0.9em}

/* Search */
.search-bar{padding:12px 16px;background:var(--panel);border-bottom:1px solid var(--border);position:sticky;top:53px;z-index:10}
.search-wrap{display:flex;align-items:center;background:var(--bg);border:1px solid var(--border);border-radius:10px;padding:0 12px;gap:8px;transition:border-color 0.2s}
.search-wrap:focus-within{border-color:var(--link)}
.search-wrap svg{width:16px;height:16px;color:var(--muted);flex-shrink:0}
.search-input{flex:1;border:none;background:none;color:var(--text);font-size:0.95em;padding:10px 0;outline:none}
.search-input::placeholder{color:var(--muted)}
.search-clear{width:28px;height:28px;border:none;background:none;color:var(--muted);cursor:pointer;font-size:1.1em;display:none;align-items:center;justify-content:center;border-radius:50%}
.search-clear:hover{background:var(--button-hover)}
.search-clear.visible{display:flex}

.tabs{display:flex;border-bottom:1px solid var(--border);background:var(--panel);position:sticky;top:109px;z-index:9}
.tab{flex:1;padding:12px;text-align:center;cursor:pointer;color:var(--muted);font-size:0.9em;border-bottom:2px solid transparent;transition:color 0.2s,border-color 0.2s}
.tab.active{color:var(--link);border-bottom-color:var(--link)}

.content-area{padding:8px}

/* Section headers */
.section-header{font-size:0.85em;font-weight:600;color:var(--muted);padding:16px 12px 8px;text-transform:uppercase;letter-spacing:0.5px;display:flex;align-items:center;gap:8px}
.section-header .count{font-weight:400;font-size:0.9em;opacity:0.7}

/* Cards */
.podcast-card{display:flex;align-items:center;padding:12px;border-radius:12px;cursor:pointer;gap:12px;min-height:64px;margin:4px 8px;background:var(--card-bg);border:1px solid var(--card-border);box-shadow:var(--card-shadow);transition:transform 0.15s,box-shadow 0.15s}
.podcast-card:active{transform:scale(0.98)}
.podcast-card:hover{box-shadow:0 2px 8px rgba(0,0,0,0.15)}
.podcast-card.playing{border-color:var(--link);box-shadow:0 0 0 1px var(--link),var(--card-shadow)}

/* Album art placeholder */
.podcast-art{width:48px;height:48px;border-radius:10px;display:flex;align-items:center;justify-content:center;font-size:1.4em;flex-shrink:0;color:#fff;position:relative}
.podcast-art .playing-indicator{position:absolute;bottom:2px;right:2px;width:12px;height:12px;background:var(--link);border-radius:50%;border:2px solid var(--card-bg);animation:pulse 1.5s infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.5}}

.podcast-info{flex:1;min-width:0}
.podcast-name{font-size:0.95em;font-weight:500;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.podcast-meta{font-size:0.75em;color:var(--muted);margin-top:2px}

/* Progress bar in cards */
.podcast-progress{height:3px;background:var(--border);border-radius:2px;margin-top:6px;overflow:hidden;position:relative}
.podcast-progress-fill{height:100%;border-radius:2px;transition:width 0.3s}
.podcast-progress-fill.partial{background:var(--link)}
.podcast-progress-fill.complete{background:var(--success)}
.podcast-badge{font-size:0.7em;color:var(--success);margin-left:4px;font-weight:600}

.podcast-actions{display:flex;gap:4px}
.add-queue-btn{width:36px;height:36px;border:none;background:var(--button-bg);color:var(--text);border-radius:50%;cursor:pointer;font-size:1em;display:flex;align-items:center;justify-content:center;transition:background 0.15s}
.add-queue-btn:hover{background:var(--button-hover)}

/* Recently played resume btn */
.resume-btn{padding:6px 14px;border:none;background:var(--link);color:#fff;border-radius:8px;cursor:pointer;font-size:0.8em;font-weight:500;white-space:nowrap;transition:opacity 0.15s}
.resume-btn:hover{opacity:0.85}

/* Folder groups */
.folder-group{margin-bottom:8px}
.folder-name{font-size:0.8em;color:var(--muted);padding:8px 12px;text-transform:uppercase;letter-spacing:0.5px}

/* Queue */
.queue-item{display:flex;align-items:center;padding:12px;border-radius:12px;gap:12px;min-height:56px;cursor:grab;margin:4px 8px;background:var(--card-bg);border:1px solid var(--card-border);box-shadow:var(--card-shadow);transition:transform 0.15s,opacity 0.2s}
.queue-item.dragging{opacity:0.4}
.queue-item .drag-handle{color:var(--muted);cursor:grab;font-size:1.2em;padding:4px}
.queue-item .remove-btn{width:32px;height:32px;border:none;background:none;color:var(--muted);cursor:pointer;font-size:1em;border-radius:50%;transition:color 0.15s,background 0.15s}
.queue-item .remove-btn:hover{color:var(--text);background:var(--button-hover)}
.empty-state{text-align:center;padding:48px 16px;color:var(--muted)}
.empty-state .empty-icon{font-size:2.5em;margin-bottom:12px;opacity:0.5}
.empty-state .empty-text{font-size:0.95em}
.empty-state .empty-sub{font-size:0.8em;margin-top:6px;opacity:0.7}

/* Skeleton loading */
.skeleton{padding:8px}
.skeleton-card{display:flex;align-items:center;padding:12px;border-radius:12px;gap:12px;margin:4px 8px;background:var(--card-bg);border:1px solid var(--card-border)}
.skeleton-art{width:48px;height:48px;border-radius:10px;background:var(--border);animation:shimmer 1.5s infinite}
.skeleton-lines{flex:1}
.skeleton-line{height:12px;border-radius:4px;background:var(--border);animation:shimmer 1.5s infinite;margin-bottom:6px}
.skeleton-line:last-child{width:60%;margin-bottom:0}
@keyframes shimmer{0%{opacity:0.5}50%{opacity:1}100%{opacity:0.5}}

/* Player bar */
.player-bar{position:fixed;bottom:0;left:0;right:0;background:var(--panel);border-top:1px solid var(--border);z-index:20;display:none;flex-direction:column;transition:transform 0.3s}
.player-bar.visible{display:flex}
.progress-container{width:100%;height:4px;background:var(--border);cursor:pointer;position:relative;transition:height 0.15s}
.progress-container:hover{height:8px}
.progress-fill{height:100%;background:var(--link);pointer-events:none;width:0%;transition:width 0.1s linear}
.player-main{display:flex;align-items:center;padding:8px 12px;gap:8px}
.player-info{flex:1;min-width:0}
.player-title{font-size:0.85em;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.player-time{font-size:0.7em;color:var(--muted)}
.player-controls{display:flex;align-items:center;gap:4px}
.player-btn{width:48px;height:48px;border:none;background:none;color:var(--text);cursor:pointer;font-size:1.3em;border-radius:50%;display:flex;align-items:center;justify-content:center;transition:background 0.15s}
.player-btn:hover{background:var(--button-hover)}
.player-btn.play-btn{font-size:1.6em}
.speed-btn{width:40px;height:32px;border:1px solid var(--border);background:var(--button-bg);color:var(--text);cursor:pointer;font-size:0.75em;font-weight:600;border-radius:6px;display:flex;align-items:center;justify-content:center;transition:background 0.15s}
.speed-btn:hover{background:var(--button-hover)}
.fs-speed-btn{padding:6px 14px;border:1px solid var(--border);background:var(--button-bg);color:var(--text);cursor:pointer;font-size:0.85em;font-weight:600;border-radius:8px;transition:background 0.15s}
.fs-speed-btn:hover{background:var(--button-hover)}

/* Fullscreen player */
.fullscreen-player{position:fixed;inset:0;background:var(--bg);z-index:30;display:none;flex-direction:column;padding:24px;transition:opacity 0.3s}
.fullscreen-player.visible{display:flex}
.fs-close{align-self:flex-start;background:none;border:none;color:var(--text);font-size:1.5em;cursor:pointer;padding:8px}
.fs-artwork{flex:1;display:flex;align-items:center;justify-content:center}
.fs-artwork .icon{width:200px;height:200px;border-radius:24px;display:flex;align-items:center;justify-content:center;font-size:5em;color:#fff;transition:transform 0.3s}
.fs-info{text-align:center;padding:24px 0}
.fs-title{font-size:1.3em;font-weight:600}
.fs-folder{font-size:0.9em;color:var(--muted);margin-top:4px}
.fs-progress{padding:0 8px}
.fs-progress-bar{width:100%;height:6px;background:var(--border);border-radius:3px;cursor:pointer;position:relative}
.fs-progress-fill{height:100%;background:var(--link);border-radius:3px;pointer-events:none;width:0%}
.fs-times{display:flex;justify-content:space-between;font-size:0.75em;color:var(--muted);margin-top:6px}
.fs-controls{display:flex;justify-content:center;align-items:center;gap:16px;padding:24px 0}
.fs-btn{width:56px;height:56px;border:none;background:none;color:var(--text);cursor:pointer;font-size:1.5em;border-radius:50%;display:flex;align-items:center;justify-content:center;transition:background 0.15s}
.fs-btn:hover{background:var(--button-hover)}
.fs-btn.play{width:72px;height:72px;font-size:2em;background:var(--link);color:#fff;border-radius:50%}

/* Touch swipe for queue items */
.queue-item.swiping{transition:none}
.queue-item.removing{transform:translateX(-100%);opacity:0;transition:transform 0.3s,opacity 0.3s}
</style>
</head>
<body>
<div class="header">
  <h1>🎧 Podcasts</h1>
  <a href="/">← Back</a>
</div>

<div class="search-bar">
  <div class="search-wrap">
    <svg viewBox="0 0 16 16" fill="currentColor"><path d="M11.5 7a4.5 4.5 0 1 1-9 0 4.5 4.5 0 0 1 9 0Zm-.82 4.74a6 6 0 1 1 1.06-1.06l3.04 3.04a.75.75 0 1 1-1.06 1.06l-3.04-3.04Z"/></svg>
    <input class="search-input" id="searchInput" type="text" placeholder="Search podcasts…" autocomplete="off"/>
    <button class="search-clear" id="searchClear">✕</button>
  </div>
</div>

<div class="tabs">
  <div class="tab active" data-tab="library">Library</div>
  <div class="tab" data-tab="queue">Queue</div>
</div>

<div class="content-area" id="contentArea">
  <div id="library">
    <div class="skeleton" id="loadingSkeleton">
      <div class="skeleton-card"><div class="skeleton-art"></div><div class="skeleton-lines"><div class="skeleton-line"></div><div class="skeleton-line"></div></div></div>
      <div class="skeleton-card"><div class="skeleton-art"></div><div class="skeleton-lines"><div class="skeleton-line"></div><div class="skeleton-line"></div></div></div>
      <div class="skeleton-card"><div class="skeleton-art"></div><div class="skeleton-lines"><div class="skeleton-line"></div><div class="skeleton-line"></div></div></div>
      <div class="skeleton-card"><div class="skeleton-art"></div><div class="skeleton-lines"><div class="skeleton-line"></div><div class="skeleton-line"></div></div></div>
    </div>
  </div>
  <div id="queueView" style="display:none"></div>
</div>

<!-- Mini player bar -->
<div class="player-bar" id="playerBar">
  <div class="progress-container" id="progressBar"><div class="progress-fill" id="progressFill"></div></div>
  <div class="player-main">
    <div class="player-info" id="playerInfoClick">
      <div class="player-title" id="playerTitle">—</div>
      <div class="player-time"><span id="playerCur">0:00</span> / <span id="playerDur">0:00</span></div>
    </div>
    <div class="player-controls">
      <button class="player-btn" id="btnRew">⏪</button>
      <button class="player-btn play-btn" id="btnPlay">▶️</button>
      <button class="player-btn" id="btnFwd">⏩</button>
      <button class="speed-btn" id="btnSpeed">1x</button>
    </div>
  </div>
</div>

<!-- Fullscreen player -->
<div class="fullscreen-player" id="fsPlayer">
  <button class="fs-close" id="fsClose">✕</button>
  <div class="fs-artwork"><div class="icon" id="fsArtwork">🎙️</div></div>
  <div class="fs-info">
    <div class="fs-title" id="fsTitle">—</div>
    <div class="fs-folder" id="fsFolder"></div>
  </div>
  <div class="fs-progress">
    <div class="fs-progress-bar" id="fsProgressBar"><div class="fs-progress-fill" id="fsProgressFill"></div></div>
    <div class="fs-times"><span id="fsCur">0:00</span><span id="fsDur">0:00</span></div>
  </div>
  <div class="fs-controls">
    <button class="fs-btn" id="fsBtnRew">⏪</button>
    <button class="fs-btn play" id="fsBtnPlay">▶️</button>
    <button class="fs-btn" id="fsBtnFwd">⏩</button>
  </div>
  <div style="text-align:center;padding-bottom:16px"><button class="fs-speed-btn" id="fsBtnSpeed">1x</button></div>
</div>

<audio id="audio" preload="metadata"></audio>

<script>
(function(){
  const audio = document.getElementById('audio');
  const playerBar = document.getElementById('playerBar');
  const playerTitle = document.getElementById('playerTitle');
  const playerCur = document.getElementById('playerCur');
  const playerDur = document.getElementById('playerDur');
  const progressFill = document.getElementById('progressFill');
  const progressBar = document.getElementById('progressBar');
  const btnPlay = document.getElementById('btnPlay');
  const btnRew = document.getElementById('btnRew');
  const btnFwd = document.getElementById('btnFwd');
  const fsPlayer = document.getElementById('fsPlayer');
  const fsTitle = document.getElementById('fsTitle');
  const fsFolder = document.getElementById('fsFolder');
  const fsArtwork = document.getElementById('fsArtwork');
  const fsCur = document.getElementById('fsCur');
  const fsDur = document.getElementById('fsDur');
  const fsProgressFill = document.getElementById('fsProgressFill');
  const fsProgressBar = document.getElementById('fsProgressBar');
  const fsBtnPlay = document.getElementById('fsBtnPlay');
  const searchInput = document.getElementById('searchInput');
  const searchClear = document.getElementById('searchClear');
  const btnSpeed = document.getElementById('btnSpeed');
  const fsBtnSpeed = document.getElementById('fsBtnSpeed');

  const SPEEDS = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2];
  let podcasts = [];
  let currentPodcast = null;
  let progress = {}; // {path: {time, lastPlayed, duration}, _speed: 1}
  let queue = [];
  let searchTerm = '';
  let currentSpeed = 1;

  function fmt(s){
    if(!s||isNaN(s))return '0:00';
    s=Math.floor(s);
    const m=Math.floor(s/60), sec=s%60;
    return m+':'+(sec<10?'0':'')+sec;
  }

  // Color from string hash for album art
  const artColors = [
    'linear-gradient(135deg,#667eea,#764ba2)',
    'linear-gradient(135deg,#f093fb,#f5576c)',
    'linear-gradient(135deg,#4facfe,#00f2fe)',
    'linear-gradient(135deg,#43e97b,#38f9d7)',
    'linear-gradient(135deg,#fa709a,#fee140)',
    'linear-gradient(135deg,#a18cd1,#fbc2eb)',
    'linear-gradient(135deg,#fccb90,#d57eeb)',
    'linear-gradient(135deg,#e0c3fc,#8ec5fc)',
    'linear-gradient(135deg,#f5576c,#ff6a88)',
    'linear-gradient(135deg,#c471f5,#fa71cd)',
    'linear-gradient(135deg,#48c6ef,#6f86d6)',
    'linear-gradient(135deg,#feada6,#f5efef)'
  ];
  function hashStr(s){let h=0;for(let i=0;i<s.length;i++){h=((h<<5)-h)+s.charCodeAt(i);h|=0;}return Math.abs(h);}
  function artBg(folder){return artColors[hashStr(folder||'root')%artColors.length];}

  // --- Server-side persistence ---
  async function loadProgress(){
    try{
      const r=await fetch('/api/podcasts/progress');
      const data=await r.json();
      // Migrate old format {path: number} to new {path: {time,lastPlayed,duration}}
      for(const[k,v] of Object.entries(data)){
        if(typeof v==='number'){
          data[k]={time:v,lastPlayed:0,duration:0};
        }
      }
      progress=data;
      if(progress._speed){currentSpeed=progress._speed;applySpeed();}
    }catch(e){progress={};}
  }

  let saveProgressTimer=null;
  async function saveProgress(){
    if(!currentPodcast||!audio.currentTime)return;
    const p=progress[currentPodcast.path]||{};
    p.time=audio.currentTime;
    p.lastPlayed=Date.now();
    if(audio.duration) p.duration=audio.duration;
    progress[currentPodcast.path]=p;
    // Debounce server writes
    if(saveProgressTimer) return;
    saveProgressTimer=setTimeout(async()=>{
      saveProgressTimer=null;
      try{await fetch('/api/podcasts/progress',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(progress)});}catch(e){}
    },500);
  }

  async function saveProgressNow(){
    if(!currentPodcast||!audio.currentTime)return;
    const p=progress[currentPodcast.path]||{};
    p.time=audio.currentTime;
    p.lastPlayed=Date.now();
    if(audio.duration) p.duration=audio.duration;
    progress[currentPodcast.path]=p;
    if(saveProgressTimer){clearTimeout(saveProgressTimer);saveProgressTimer=null;}
    try{await fetch('/api/podcasts/progress',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(progress)});}catch(e){}
  }

  async function loadQueue(){
    try{const r=await fetch('/api/podcasts/queue');queue=await r.json();}catch(e){queue=[];}
  }

  async function saveQueue(){
    try{await fetch('/api/podcasts/queue',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(queue)});}catch(e){}
  }

  async function loadPodcasts(){
    const r=await fetch('/api/podcasts');
    podcasts=await r.json();
    renderAll();
  }

  // --- Progress helpers ---
  function getProgressPct(path){
    const p=progress[path];
    if(!p||!p.duration||p.duration<=0) return -1;
    return Math.min(100, Math.round(p.time/p.duration*100));
  }

  function getRecentlyPlayed(){
    return Object.entries(progress)
      .filter(([_,v])=>v.lastPlayed>0)
      .sort((a,b)=>b[1].lastPlayed-a[1].lastPlayed)
      .slice(0,10)
      .map(([path,v])=>{
        const pod=podcasts.find(x=>x.path===path);
        return pod?{...pod,progress:v}:null;
      })
      .filter(Boolean);
  }

  function matchesSearch(p){
    if(!searchTerm)return true;
    const t=searchTerm.toLowerCase();
    return (p.name||'').toLowerCase().includes(t)||(p.folder||'').toLowerCase().includes(t);
  }

  // --- Render ---
  function renderAll(){
    renderLibrary();
    renderQueue();
  }

  function progressBarHTML(path){
    const pct=getProgressPct(path);
    if(pct<0) return '';
    if(pct>=100) return '<span class="podcast-badge">✓</span>';
    return '<div class="podcast-progress"><div class="podcast-progress-fill partial" style="width:'+pct+'%"></div></div>';
  }

  function podcastCardHTML(p,opts){
    const playing=currentPodcast&&currentPodcast.path===p.path;
    const d=new Date(p.modifiedAt*1000);
    const ds=d.toLocaleDateString(undefined,{month:'short',day:'numeric',year:'numeric'});
    const sz=(p.size/(1024*1024)).toFixed(1)+'MB';
    const pct=getProgressPct(p.path);
    const bg=artBg(p.folder);
    let html='<div class="podcast-card'+(playing?' playing':'')+'" data-path="'+esc(p.path)+'">';
    html+='<div class="podcast-art" style="background:'+bg+'">';
    html+=(playing?'🔊':'🎙️');
    if(playing) html+='<div class="playing-indicator"></div>';
    html+='</div>';
    html+='<div class="podcast-info"><div class="podcast-name">'+esc(p.name);
    if(pct>=100) html+='<span class="podcast-badge">✓</span>';
    html+='</div>';
    html+='<div class="podcast-meta">'+ds+' · '+sz;
    if(opts&&opts.showFolder&&p.folder) html+=' · '+esc(p.folder);
    if(pct>0&&pct<100) html+=' · '+pct+'%';
    html+='</div>';
    if(pct>0&&pct<100) html+='<div class="podcast-progress"><div class="podcast-progress-fill partial" style="width:'+pct+'%"></div></div>';
    html+='</div>';
    if(opts&&opts.resume){
      html+='<button class="resume-btn" data-path="'+esc(p.path)+'">Resume</button>';
    }else{
      html+='<div class="podcast-actions"><button class="add-queue-btn" data-path="'+esc(p.path)+'" title="Add to queue">+</button></div>';
    }
    html+='</div>';
    return html;
  }

  function renderLibrary(){
    const lib=document.getElementById('library');
    const filtered=podcasts.filter(matchesSearch);
    const recentlyPlayed=searchTerm?[]:getRecentlyPlayed();

    let html='';

    // Recently Played section
    if(recentlyPlayed.length>0){
      html+='<div class="section-header">⏱ Recently Played <span class="count">('+recentlyPlayed.length+')</span></div>';
      recentlyPlayed.forEach(p=>{
        html+=podcastCardHTML(p,{showFolder:true,resume:true});
      });
    }

    // Library section
    if(!filtered.length&&podcasts.length){
      html+='<div class="empty-state"><div class="empty-icon">🔍</div><div class="empty-text">No podcasts match "'+esc(searchTerm)+'"</div><div class="empty-sub">Try a different search term</div></div>';
    }else if(!filtered.length){
      html+='<div class="empty-state"><div class="empty-icon">🎙️</div><div class="empty-text">No podcasts found</div><div class="empty-sub">Generate podcasts from markdown files first</div></div>';
    }else{
      html+='<div class="section-header">📚 Library <span class="count">('+filtered.length+')</span></div>';
      const groups={};
      filtered.forEach(p=>{const f=p.folder||'Root';(groups[f]=groups[f]||[]).push(p);});
      for(const[folder,items] of Object.entries(groups)){
        html+='<div class="folder-group"><div class="folder-name">'+esc(folder)+'</div>';
        items.forEach(p=>{html+=podcastCardHTML(p,{});});
        html+='</div>';
      }
    }
    lib.innerHTML=html;

    // Bind events
    lib.querySelectorAll('.podcast-card').forEach(el=>{
      el.addEventListener('click',e=>{
        if(e.target.closest('.add-queue-btn')||e.target.closest('.resume-btn'))return;
        playPodcast(el.dataset.path);
      });
    });
    lib.querySelectorAll('.add-queue-btn').forEach(btn=>{
      btn.addEventListener('click',e=>{
        e.stopPropagation();
        const p=podcasts.find(x=>x.path===btn.dataset.path);
        if(p&&!queue.find(q=>q.path===p.path)){queue.push(p);saveQueue();renderQueue();btn.textContent='✓';setTimeout(()=>btn.textContent='+',800);}
      });
    });
    lib.querySelectorAll('.resume-btn').forEach(btn=>{
      btn.addEventListener('click',e=>{
        e.stopPropagation();
        playPodcast(btn.dataset.path);
      });
    });
  }

  function renderQueue(){
    const qv=document.getElementById('queueView');
    if(!queue.length){
      qv.innerHTML='<div class="empty-state"><div class="empty-icon">📋</div><div class="empty-text">Queue is empty</div><div class="empty-sub">Tap + on a podcast to add it</div></div>';
      return;
    }
    let html='<div class="section-header">🎵 Up Next <span class="count">('+queue.length+')</span></div>';
    queue.forEach((p,i)=>{
      const bg=artBg(p.folder);
      html+='<div class="queue-item" draggable="true" data-idx="'+i+'">';
      html+='<span class="drag-handle">☰</span>';
      html+='<div class="podcast-art" style="background:'+bg+';width:40px;height:40px;font-size:1.1em">🎙️</div>';
      html+='<div class="podcast-info" style="flex:1;min-width:0"><div class="podcast-name">'+esc(p.name)+'</div><div class="podcast-meta">'+esc(p.folder||'Root')+'</div></div>';
      html+='<button class="remove-btn" data-idx="'+i+'">✕</button>';
      html+='</div>';
    });
    qv.innerHTML=html;

    // Drag and drop
    let dragIdx=null;
    qv.querySelectorAll('.queue-item').forEach(el=>{
      el.addEventListener('dragstart',e=>{dragIdx=+el.dataset.idx;el.classList.add('dragging');});
      el.addEventListener('dragend',()=>{el.classList.remove('dragging');});
      el.addEventListener('dragover',e=>{e.preventDefault();});
      el.addEventListener('drop',e=>{
        e.preventDefault();
        const toIdx=+el.dataset.idx;
        if(dragIdx!==null&&dragIdx!==toIdx){
          const item=queue.splice(dragIdx,1)[0];
          queue.splice(toIdx,0,item);
          saveQueue();renderQueue();
        }
      });

      // Touch swipe to remove
      let startX=0,curX=0,swiping=false;
      el.addEventListener('touchstart',e=>{
        if(e.target.closest('.remove-btn')||e.target.closest('.drag-handle'))return;
        startX=e.touches[0].clientX;swiping=true;el.classList.add('swiping');
      },{passive:true});
      el.addEventListener('touchmove',e=>{
        if(!swiping)return;
        curX=e.touches[0].clientX;
        const dx=curX-startX;
        if(dx<0) el.style.transform='translateX('+dx+'px)';
      },{passive:true});
      el.addEventListener('touchend',()=>{
        if(!swiping)return;
        swiping=false;el.classList.remove('swiping');
        const dx=curX-startX;
        if(dx<-100){
          el.classList.add('removing');
          setTimeout(()=>{queue.splice(+el.dataset.idx,1);saveQueue();renderQueue();},300);
        }else{
          el.style.transform='';
        }
        curX=0;
      });
    });

    qv.querySelectorAll('.remove-btn').forEach(btn=>{
      btn.addEventListener('click',()=>{queue.splice(+btn.dataset.idx,1);saveQueue();renderQueue();});
    });
    // Tap to play
    qv.querySelectorAll('.queue-item').forEach(el=>{
      el.addEventListener('click',e=>{
        if(e.target.closest('.remove-btn')||e.target.closest('.drag-handle'))return;
        const p=queue[+el.dataset.idx];
        if(p) playPodcast(p.path);
      });
    });
  }

  function playPodcast(path){
    const p=podcasts.find(x=>x.path===path);
    if(!p)return;
    currentPodcast=p;
    audio.src='/api/media/'+encodeURIComponent(p.path).replace(/%2F/g,'/');
    const saved=progress[p.path];
    audio.addEventListener('loadedmetadata',function once(){
      if(saved&&saved.time>0) audio.currentTime=saved.time;
      audio.removeEventListener('loadedmetadata',once);
    });
    // Mark as played immediately
    if(!progress[p.path]) progress[p.path]={time:0,lastPlayed:Date.now(),duration:0};
    else progress[p.path].lastPlayed=Date.now();

    audio.play();
    audio.playbackRate=currentSpeed;
    playerBar.classList.add('visible');
    playerTitle.textContent=p.name;
    fsTitle.textContent=p.name;
    fsFolder.textContent=p.folder||'';
    fsArtwork.style.background=artBg(p.folder);
    updatePlayBtn();
    renderAll();
    saveProgress();
  }

  function updatePlayBtn(){
    const icon=audio.paused?'▶️':'⏸️';
    btnPlay.textContent=icon;
    fsBtnPlay.textContent=icon;
  }

  function applySpeed(){
    audio.playbackRate=currentSpeed;
    const label=currentSpeed===1?'1x':currentSpeed+'x';
    btnSpeed.textContent=label;
    fsBtnSpeed.textContent=label;
  }
  function cycleSpeed(){
    const idx=SPEEDS.indexOf(currentSpeed);
    currentSpeed=SPEEDS[(idx+1)%SPEEDS.length];
    applySpeed();
    progress._speed=currentSpeed;
    saveProgress();
  }
  btnSpeed.onclick=cycleSpeed;
  fsBtnSpeed.onclick=cycleSpeed;

  btnPlay.onclick=()=>{audio.paused?audio.play():audio.pause();};
  fsBtnPlay.onclick=()=>{audio.paused?audio.play():audio.pause();};
  btnRew.onclick=()=>{audio.currentTime=Math.max(0,audio.currentTime-15);};
  btnFwd.onclick=()=>{audio.currentTime=Math.min(audio.duration||0,audio.currentTime+15);};
  document.getElementById('fsBtnRew').onclick=btnRew.onclick;
  document.getElementById('fsBtnFwd').onclick=btnFwd.onclick;

  audio.addEventListener('play', updatePlayBtn);
  audio.addEventListener('pause', updatePlayBtn);
  audio.addEventListener('timeupdate',()=>{
    const pct=audio.duration?(audio.currentTime/audio.duration*100):0;
    progressFill.style.width=pct+'%';
    fsProgressFill.style.width=pct+'%';
    playerCur.textContent=fmt(audio.currentTime);
    playerDur.textContent=fmt(audio.duration);
    fsCur.textContent=fmt(audio.currentTime);
    fsDur.textContent=fmt(audio.duration);
  });

  // Save progress every 5s
  setInterval(saveProgress, 5000);
  audio.addEventListener('pause', ()=>{saveProgressNow();renderLibrary();});

  // Auto-play next in queue
  audio.addEventListener('ended',()=>{
    saveProgressNow();
    if(!currentPodcast)return;
    const qi=queue.findIndex(q=>q.path===currentPodcast.path);
    if(qi>=0&&qi<queue.length-1){playPodcast(queue[qi+1].path);}
    else{renderLibrary();}
  });

  // Seek
  function seek(bar,fill,e){
    const rect=bar.getBoundingClientRect();
    const pct=Math.max(0,Math.min(1,(e.clientX-rect.left)/rect.width));
    if(audio.duration) audio.currentTime=pct*audio.duration;
  }
  [progressBar,fsProgressBar].forEach(bar=>{
    const fill=bar===progressBar?progressFill:fsProgressFill;
    let dragging=false;
    bar.addEventListener('mousedown',e=>{dragging=true;seek(bar,fill,e);});
    bar.addEventListener('touchstart',e=>{dragging=true;seek(bar,fill,e.touches[0]);},{passive:true});
    document.addEventListener('mousemove',e=>{if(dragging)seek(bar,fill,e);});
    document.addEventListener('touchmove',e=>{if(dragging)seek(bar,fill,e.touches[0]);},{passive:true});
    document.addEventListener('mouseup',()=>{dragging=false;});
    document.addEventListener('touchend',()=>{dragging=false;});
  });

  // Fullscreen player
  document.getElementById('playerInfoClick').addEventListener('click',()=>fsPlayer.classList.add('visible'));
  document.getElementById('fsClose').addEventListener('click',()=>fsPlayer.classList.remove('visible'));

  // Tabs
  document.querySelectorAll('.tab').forEach(t=>{
    t.addEventListener('click',()=>{
      document.querySelectorAll('.tab').forEach(x=>x.classList.remove('active'));
      t.classList.add('active');
      document.getElementById('library').style.display=t.dataset.tab==='library'?'':'none';
      document.getElementById('queueView').style.display=t.dataset.tab==='queue'?'':'none';
      if(t.dataset.tab==='queue') renderQueue();
    });
  });

  // Search
  searchInput.addEventListener('input',()=>{
    searchTerm=searchInput.value.trim();
    searchClear.classList.toggle('visible',searchTerm.length>0);
    renderLibrary();
  });
  searchClear.addEventListener('click',()=>{
    searchInput.value='';searchTerm='';searchClear.classList.remove('visible');renderLibrary();searchInput.focus();
  });

  function esc(s){const d=document.createElement('div');d.textContent=s;return d.innerHTML;}

  // Init
  Promise.all([loadProgress(),loadQueue()]).then(()=>loadPodcasts());
})();
</script>
</body>
</html>
`
