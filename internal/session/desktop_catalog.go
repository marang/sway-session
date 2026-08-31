package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	MaxDesktopSearchDirectories = 64
	MaxDesktopEntries           = 4096
	MaxDesktopFilesystemEntries = 16384
	MaxDesktopEntrySize         = 1024 * 1024
	MaxDesktopDirectoryDepth    = 64
	desktopReadBatchSize        = 256
)

// DesktopSearchDirectory is one XDG data-root applications directory in
// precedence order. Origin is trust metadata only; file ownership and
// mutability are revalidated before registration and launch.
type DesktopSearchDirectory struct {
	Path                string
	Origin              DesktopEntryOrigin
	FlatpakInstallation FlatpakInstallation
}

// DesktopEntry is an immutable in-memory catalog record. Exec is deliberately
// not part of persistent session state and is never interpreted by this
// package; the trusted launcher adapter delegates desktop-entry execution to
// GIO.
type DesktopEntry struct {
	ID                  string
	Name                string
	Icon                string
	Path                string
	Origin              DesktopEntryOrigin
	Exec                string `json:"-"`
	TryExec             string `json:"-"`
	StartupWMClass      string
	FlatpakID           string
	FlatpakInstallation FlatpakInstallation
	NoDisplay           bool
	Terminal            bool
	DBusActivatable     bool
	SingleMainWindow    bool
}

type DesktopCatalogIssue struct {
	Path   string
	Reason string
}

// DesktopCatalog is one cached, precedence-resolved XDG desktop-entry
// snapshot. Its indexes are private so callers cannot mutate cached state.
type DesktopCatalog struct {
	entries          []DesktopEntry
	issues           []DesktopCatalogIssue
	byID             map[string]DesktopEntry
	byStartupWMClass map[string][]DesktopEntry
	byFlatpakID      map[string][]DesktopEntry
}

func (catalog DesktopCatalog) Entries() []DesktopEntry {
	return append([]DesktopEntry(nil), catalog.entries...)
}

func (catalog DesktopCatalog) Issues() []DesktopCatalogIssue {
	return append([]DesktopCatalogIssue(nil), catalog.issues...)
}

func (catalog DesktopCatalog) ByID(id string) (DesktopEntry, bool) {
	entry, exists := catalog.byID[id]
	return entry, exists
}

func (catalog DesktopCatalog) ByStartupWMClass(class string) []DesktopEntry {
	return append([]DesktopEntry(nil), catalog.byStartupWMClass[class]...)
}

func (catalog DesktopCatalog) ByFlatpakID(id string) []DesktopEntry {
	return append([]DesktopEntry(nil), catalog.byFlatpakID[id]...)
}

// DesktopCatalogCache holds one immutable catalog until explicitly
// invalidated. LAB-97 can connect invalidation to registration-time filesystem
// observation without changing the resolver boundary.
type DesktopCatalogCache struct {
	mu      sync.Mutex
	search  []DesktopSearchDirectory
	loaded  bool
	catalog DesktopCatalog
}

func NewDesktopCatalogCache(search []DesktopSearchDirectory) *DesktopCatalogCache {
	return &DesktopCatalogCache{search: append([]DesktopSearchDirectory(nil), search...)}
}

func (cache *DesktopCatalogCache) Load() (DesktopCatalog, error) {
	if cache == nil {
		return DesktopCatalog{}, errors.New("desktop catalog cache is nil")
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.loaded {
		return cache.catalog, nil
	}
	catalog, err := LoadDesktopCatalog(cache.search)
	if err != nil {
		return DesktopCatalog{}, err
	}
	cache.catalog = catalog
	cache.loaded = true
	return catalog, nil
}

func (cache *DesktopCatalogCache) Invalidate() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.loaded = false
	cache.catalog = DesktopCatalog{}
}

// DefaultDesktopSearchPath resolves the XDG applications directories in
// desktop-file precedence order.
func DefaultDesktopSearchPath() ([]DesktopSearchDirectory, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	dataDirectories := os.Getenv("XDG_DATA_DIRS")
	if dataDirectories == "" {
		dataDirectories = "/usr/local/share:/usr/share"
	}
	roots := append([]string{dataHome}, filepath.SplitList(dataDirectories)...)
	if len(roots) > MaxDesktopSearchDirectories {
		return nil, fmt.Errorf("desktop search path contains %d directories; maximum is %d", len(roots), MaxDesktopSearchDirectories)
	}
	search := make([]DesktopSearchDirectory, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) || containsControl(root) {
			return nil, fmt.Errorf("XDG data directory must be an absolute path without control characters: %q", root)
		}
		root = filepath.Clean(root)
		applications := filepath.Join(root, "applications")
		if _, exists := seen[applications]; exists {
			continue
		}
		seen[applications] = struct{}{}
		origin := DesktopEntrySystem
		if pathWithin(home, root) {
			origin = DesktopEntryUser
		}
		installation := FlatpakInstallation("")
		if strings.HasSuffix(filepath.ToSlash(root), "/flatpak/exports/share") {
			if origin == DesktopEntryUser {
				installation = FlatpakUser
			} else {
				installation = FlatpakSystem
			}
		}
		search = append(search, DesktopSearchDirectory{
			Path:                applications,
			Origin:              origin,
			FlatpakInstallation: installation,
		})
	}
	return search, nil
}

func pathWithin(parent string, candidate string) bool {
	relative, err := filepath.Rel(parent, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// LoadDesktopCatalog reads a bounded immutable desktop-entry snapshot. A
// malformed higher-precedence entry still claims its desktop ID, preventing an
// unexpected lower-precedence launcher from being selected instead.
func LoadDesktopCatalog(search []DesktopSearchDirectory) (DesktopCatalog, error) {
	return loadDesktopCatalog(search, MaxDesktopFilesystemEntries)
}

func loadDesktopCatalog(search []DesktopSearchDirectory, filesystemLimit int) (DesktopCatalog, error) {
	if len(search) > MaxDesktopSearchDirectories {
		return DesktopCatalog{}, fmt.Errorf("desktop search path contains %d directories; maximum is %d", len(search), MaxDesktopSearchDirectories)
	}
	if filesystemLimit <= 0 || filesystemLimit > MaxDesktopFilesystemEntries {
		return DesktopCatalog{}, fmt.Errorf("desktop catalog filesystem limit must be between 1 and %d", MaxDesktopFilesystemEntries)
	}
	entries := make(map[string]DesktopEntry)
	claimed := make(map[string]struct{})
	issues := make([]DesktopCatalogIssue, 0)
	scanned := 0
	visited := 0
	for _, directory := range search {
		if err := validateDesktopSearchDirectory(directory); err != nil {
			return DesktopCatalog{}, err
		}
		paths, walkIssues, directoryVisited, err := desktopFiles(directory.Path, filesystemLimit-visited)
		issues = append(issues, walkIssues...)
		if err != nil {
			return DesktopCatalog{}, fmt.Errorf("load desktop search directory: %w (maximum %d filesystem entries)", err, filesystemLimit)
		}
		visited += directoryVisited
		local := make(map[string]string, len(paths))
		for _, path := range paths {
			scanned++
			if scanned > MaxDesktopEntries {
				return DesktopCatalog{}, fmt.Errorf("desktop catalog exceeds %d entries", MaxDesktopEntries)
			}
			id, err := desktopFileID(directory.Path, path)
			if err != nil {
				issues = append(issues, DesktopCatalogIssue{Path: path, Reason: err.Error()})
				continue
			}
			if previous, duplicate := local[id]; duplicate {
				if existing, exists := entries[id]; exists && existing.Path == previous {
					delete(entries, id)
				}
				claimed[id] = struct{}{}
				issues = append(issues,
					DesktopCatalogIssue{Path: previous, Reason: "desktop file ID collides within one XDG data directory"},
					DesktopCatalogIssue{Path: path, Reason: "desktop file ID collides within one XDG data directory"},
				)
				continue
			}
			local[id] = path
			if _, shadowed := claimed[id]; shadowed {
				continue
			}
			claimed[id] = struct{}{}
			data, err := readDesktopEntry(path)
			if err != nil {
				issues = append(issues, DesktopCatalogIssue{Path: path, Reason: err.Error()})
				continue
			}
			parsed, hidden, err := parseDesktopEntry(data)
			if err != nil {
				issues = append(issues, DesktopCatalogIssue{Path: path, Reason: err.Error()})
				continue
			}
			if hidden {
				continue
			}
			parsed.ID = id
			parsed.Path = path
			parsed.Origin = directory.Origin
			if parsed.FlatpakID != "" {
				parsed.FlatpakInstallation = directory.FlatpakInstallation
			}
			entries[id] = parsed
		}
	}
	return buildDesktopCatalog(entries, issues), nil
}

func validateDesktopSearchDirectory(directory DesktopSearchDirectory) error {
	if directory.Path == "" || !filepath.IsAbs(directory.Path) || filepath.Clean(directory.Path) != directory.Path || containsControl(directory.Path) {
		return errors.New("desktop search directory must be a clean absolute path")
	}
	switch directory.Origin {
	case DesktopEntrySystem, DesktopEntryUser:
	default:
		return fmt.Errorf("unsupported desktop search origin %q", directory.Origin)
	}
	switch directory.FlatpakInstallation {
	case "", FlatpakSystem, FlatpakUser:
	default:
		return fmt.Errorf("unsupported desktop search Flatpak installation %q", directory.FlatpakInstallation)
	}
	return nil
}

func desktopFiles(root string, remaining int) ([]string, []DesktopCatalogIssue, int, error) {
	paths := make([]string, 0)
	issues := make([]DesktopCatalogIssue, 0)
	if remaining <= 0 {
		return nil, issues, 0, errors.New("desktop catalog filesystem entries budget is exhausted")
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return paths, issues, 0, nil
	}
	if err != nil {
		issues = append(issues, DesktopCatalogIssue{Path: root, Reason: err.Error()})
		return paths, issues, 0, nil
	}
	directory := os.NewFile(uintptr(fd), root)
	if directory == nil {
		_ = unix.Close(fd)
		return nil, issues, 0, errors.New("open desktop search root: invalid file descriptor")
	}
	defer directory.Close()
	visited := 1
	var walk func(*os.File, string, int) error
	walk = func(opened *os.File, directoryPath string, depth int) error {
		for {
			entries, readErr := opened.ReadDir(desktopReadBatchSize)
			for _, entry := range entries {
				visited++
				if visited > remaining {
					return errors.New("desktop catalog filesystem entries budget is exhausted")
				}
				path := filepath.Join(directoryPath, entry.Name())
				info, infoErr := entry.Info()
				if infoErr != nil {
					issues = append(issues, DesktopCatalogIssue{Path: path, Reason: infoErr.Error()})
					continue
				}
				if info.IsDir() {
					if depth >= MaxDesktopDirectoryDepth {
						issues = append(issues, DesktopCatalogIssue{Path: path, Reason: fmt.Sprintf("desktop directory nesting exceeds %d levels", MaxDesktopDirectoryDepth)})
						continue
					}
					childFD, openErr := unix.Openat(int(opened.Fd()), entry.Name(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
					if openErr != nil {
						issues = append(issues, DesktopCatalogIssue{Path: path, Reason: openErr.Error()})
						continue
					}
					child := os.NewFile(uintptr(childFD), path)
					if child == nil {
						_ = unix.Close(childFD)
						return errors.New("open desktop search directory: invalid file descriptor")
					}
					walkErr := walk(child, path, depth+1)
					closeErr := child.Close()
					if walkErr != nil {
						return walkErr
					}
					if closeErr != nil {
						issues = append(issues, DesktopCatalogIssue{Path: path, Reason: closeErr.Error()})
					}
					continue
				}
				if strings.HasSuffix(entry.Name(), ".desktop") {
					paths = append(paths, path)
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				issues = append(issues, DesktopCatalogIssue{Path: directoryPath, Reason: readErr.Error()})
				return nil
			}
			if len(entries) == 0 {
				return errors.New("read desktop search directory returned no entries without completion")
			}
		}
	}
	if err := walk(directory, root, 0); err != nil {
		return nil, issues, visited, err
	}
	sort.Strings(paths)
	return paths, issues, visited, nil
}

func desktopFileID(root string, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("desktop file is outside its XDG applications directory")
	}
	id := strings.ReplaceAll(filepath.ToSlash(relative), "/", "-")
	if err := validateDesktopID(id); err != nil {
		return "", err
	}
	return id, nil
}

func readDesktopEntry(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open desktop entry: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open desktop entry: invalid file descriptor")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect desktop entry: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("desktop entry must resolve to a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxDesktopEntrySize+1))
	if err != nil {
		return nil, fmt.Errorf("read desktop entry: %w", err)
	}
	if len(data) > MaxDesktopEntrySize {
		return nil, fmt.Errorf("desktop entry exceeds %d bytes", MaxDesktopEntrySize)
	}
	return data, nil
}

func parseDesktopEntry(data []byte) (DesktopEntry, bool, error) {
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return DesktopEntry{}, false, errors.New("desktop entry must contain valid UTF-8 without NUL bytes")
	}
	values := make(map[string]string)
	inDesktopEntry := false
	foundDesktopEntry := false
	for lineNumber, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			group := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			inDesktopEntry = group == "Desktop Entry"
			if inDesktopEntry {
				if foundDesktopEntry {
					return DesktopEntry{}, false, errors.New("desktop entry contains multiple [Desktop Entry] groups")
				}
				foundDesktopEntry = true
			}
			continue
		}
		if !inDesktopEntry {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key = strings.Trim(key, " \t")
		value = strings.Trim(value, " \t")
		if !found || key == "" {
			return DesktopEntry{}, false, fmt.Errorf("desktop entry line %d is not a valid key/value", lineNumber+1)
		}
		if !catalogKey(key) {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return DesktopEntry{}, false, fmt.Errorf("desktop entry contains duplicate key %s", key)
		}
		values[key] = value
	}
	if !foundDesktopEntry {
		return DesktopEntry{}, false, errors.New("desktop entry is missing [Desktop Entry]")
	}
	hidden, err := desktopBoolean(values, "Hidden")
	if err != nil {
		return DesktopEntry{}, false, err
	}
	if hidden {
		return DesktopEntry{}, true, nil
	}
	if values["Type"] != "Application" {
		return DesktopEntry{}, false, errors.New("desktop entry Type must be Application")
	}
	name, err := unescapeDesktopString(values["Name"])
	if err != nil || name == "" || len(name) > 1024 || containsControl(name) {
		return DesktopEntry{}, false, errors.New("desktop entry Name must be a bounded display string")
	}
	dbus, err := desktopBoolean(values, "DBusActivatable")
	if err != nil {
		return DesktopEntry{}, false, err
	}
	if values["Exec"] == "" && !dbus {
		return DesktopEntry{}, false, errors.New("desktop entry requires Exec unless DBusActivatable is true")
	}
	noDisplay, err := desktopBoolean(values, "NoDisplay")
	if err != nil {
		return DesktopEntry{}, false, err
	}
	terminal, err := desktopBoolean(values, "Terminal")
	if err != nil {
		return DesktopEntry{}, false, err
	}
	singleMainWindow, err := desktopBoolean(values, "SingleMainWindow")
	if err != nil {
		return DesktopEntry{}, false, err
	}
	startupWMClass, err := unescapeDesktopString(values["StartupWMClass"])
	if err != nil {
		return DesktopEntry{}, false, fmt.Errorf("decode StartupWMClass: %w", err)
	}
	tryExec, err := unescapeDesktopString(values["TryExec"])
	if err != nil {
		return DesktopEntry{}, false, fmt.Errorf("decode TryExec: %w", err)
	}
	icon, err := unescapeDesktopString(values["Icon"])
	if err != nil {
		return DesktopEntry{}, false, fmt.Errorf("decode Icon: %w", err)
	}
	flatpakID, err := unescapeDesktopString(values["X-Flatpak"])
	if err != nil {
		return DesktopEntry{}, false, fmt.Errorf("decode X-Flatpak: %w", err)
	}
	for field, value := range map[string]string{
		"StartupWMClass": startupWMClass,
		"TryExec":        tryExec,
		"Icon":           icon,
		"X-Flatpak":      flatpakID,
	} {
		if len(value) > 1024 || containsControl(value) {
			return DesktopEntry{}, false, fmt.Errorf("desktop entry %s must be bounded and contain no control characters", field)
		}
	}
	if flatpakID != "" && !validFlatpakID(flatpakID) {
		return DesktopEntry{}, false, errors.New("desktop entry X-Flatpak must be a valid application ID")
	}
	return DesktopEntry{
		Name:             name,
		Icon:             icon,
		Exec:             values["Exec"],
		TryExec:          tryExec,
		StartupWMClass:   startupWMClass,
		FlatpakID:        flatpakID,
		NoDisplay:        noDisplay,
		Terminal:         terminal,
		DBusActivatable:  dbus,
		SingleMainWindow: singleMainWindow,
	}, false, nil
}

func catalogKey(key string) bool {
	if strings.HasPrefix(key, "Name[") {
		return false
	}
	switch key {
	case "Type", "Name", "Icon", "Exec", "TryExec", "StartupWMClass", "X-Flatpak",
		"Hidden", "NoDisplay", "Terminal", "DBusActivatable", "SingleMainWindow":
		return true
	default:
		return false
	}
}

func desktopBoolean(values map[string]string, key string) (bool, error) {
	value, exists := values[key]
	if !exists {
		return false, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("desktop entry %s must be true or false", key)
	}
}

func unescapeDesktopString(value string) (string, error) {
	if !strings.Contains(value, "\\") {
		return value, nil
	}
	var result strings.Builder
	result.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			continue
		}
		index++
		if index == len(value) {
			return "", errors.New("desktop string ends with an escape")
		}
		switch value[index] {
		case 's':
			result.WriteByte(' ')
		case 'n':
			result.WriteByte('\n')
		case 't':
			result.WriteByte('\t')
		case 'r':
			result.WriteByte('\r')
		case '\\':
			result.WriteByte('\\')
		default:
			return "", fmt.Errorf("unsupported desktop string escape \\%c", value[index])
		}
	}
	return result.String(), nil
}

func buildDesktopCatalog(entries map[string]DesktopEntry, issues []DesktopCatalogIssue) DesktopCatalog {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	catalog := DesktopCatalog{
		entries:          make([]DesktopEntry, 0, len(ids)),
		issues:           append([]DesktopCatalogIssue(nil), issues...),
		byID:             make(map[string]DesktopEntry, len(ids)),
		byStartupWMClass: make(map[string][]DesktopEntry),
		byFlatpakID:      make(map[string][]DesktopEntry),
	}
	for _, id := range ids {
		entry := entries[id]
		catalog.entries = append(catalog.entries, entry)
		catalog.byID[id] = entry
		if entry.StartupWMClass != "" {
			catalog.byStartupWMClass[entry.StartupWMClass] = append(catalog.byStartupWMClass[entry.StartupWMClass], entry)
		}
		if entry.FlatpakID != "" {
			catalog.byFlatpakID[entry.FlatpakID] = append(catalog.byFlatpakID[entry.FlatpakID], entry)
		}
	}
	sort.Slice(catalog.issues, func(left int, right int) bool {
		if catalog.issues[left].Path == catalog.issues[right].Path {
			return catalog.issues[left].Reason < catalog.issues[right].Reason
		}
		return catalog.issues[left].Path < catalog.issues[right].Path
	})
	return catalog
}
