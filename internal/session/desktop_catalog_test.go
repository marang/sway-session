package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDefaultDesktopSearchPathUsesXDGPrecedenceAndClassifiesFlatpak(t *testing.T) {
	home := t.TempDir()
	dataHome := filepath.Join(home, "data")
	flatpakRoot := filepath.Join(home, ".local", "share", "flatpak", "exports", "share")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataHome+string(os.PathSeparator))
	t.Setenv("XDG_DATA_DIRS", strings.Join([]string{flatpakRoot, "/usr/share", "/usr/share"}, string(os.PathListSeparator)))

	search, err := DefaultDesktopSearchPath()
	if err != nil {
		t.Fatalf("resolve desktop search path: %v", err)
	}
	want := []DesktopSearchDirectory{
		{Path: filepath.Join(dataHome, "applications"), Origin: DesktopEntryUser},
		{Path: filepath.Join(flatpakRoot, "applications"), Origin: DesktopEntryUser, FlatpakInstallation: FlatpakUser},
		{Path: "/usr/share/applications", Origin: DesktopEntrySystem},
	}
	if len(search) != len(want) {
		t.Fatalf("unexpected search path length: got=%+v want=%+v", search, want)
	}
	for index := range want {
		if search[index] != want[index] {
			t.Fatalf("search[%d]=%+v want=%+v", index, search[index], want[index])
		}
	}
}

func TestDesktopCatalogAppliesXDGPrecedenceAndHiddenTombstones(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user", "applications")
	system := filepath.Join(root, "system", "applications")
	writeDesktopTestFile(t, filepath.Join(user, "same.desktop"), desktopTestEntry("User App", "user-app", ""))
	writeDesktopTestFile(t, filepath.Join(system, "same.desktop"), desktopTestEntry("System App", "system-app", ""))
	writeDesktopTestFile(t, filepath.Join(user, "hidden.desktop"), "[Desktop Entry]\nHidden=true\n")
	writeDesktopTestFile(t, filepath.Join(system, "hidden.desktop"), desktopTestEntry("Must Stay Hidden", "hidden", ""))
	writeDesktopTestFile(t, filepath.Join(system, "other.desktop"), desktopTestEntry("Other", "other", ""))

	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{
		{Path: user, Origin: DesktopEntryUser},
		{Path: system, Origin: DesktopEntrySystem},
	})
	if err != nil {
		t.Fatalf("load desktop catalog: %v", err)
	}
	if got, exists := catalog.ByID("same.desktop"); !exists || got.Name != "User App" || got.Origin != DesktopEntryUser {
		t.Fatalf("higher-precedence desktop entry was not selected: %+v exists=%t", got, exists)
	}
	if _, exists := catalog.ByID("hidden.desktop"); exists {
		t.Fatal("Hidden desktop entry did not shadow lower-precedence entry")
	}
	if got, exists := catalog.ByID("other.desktop"); !exists || got.Name != "Other" {
		t.Fatalf("independent system entry missing: %+v exists=%t", got, exists)
	}
	if len(catalog.Entries()) != 2 {
		t.Fatalf("unexpected resolved catalog: %+v", catalog.Entries())
	}
}

func TestDesktopCatalogMalformedHigherEntryFailsClosedForItsID(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user", "applications")
	system := filepath.Join(root, "system", "applications")
	writeDesktopTestFile(t, filepath.Join(user, "same.desktop"), "[Desktop Entry]\nType=Application\nName=Broken\n")
	writeDesktopTestFile(t, filepath.Join(system, "same.desktop"), desktopTestEntry("System App", "system-app", ""))

	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{
		{Path: user, Origin: DesktopEntryUser},
		{Path: system, Origin: DesktopEntrySystem},
	})
	if err != nil {
		t.Fatalf("load desktop catalog: %v", err)
	}
	if _, exists := catalog.ByID("same.desktop"); exists {
		t.Fatal("malformed higher-precedence entry unexpectedly fell back to system launcher")
	}
	if len(catalog.Issues()) != 1 || !strings.Contains(catalog.Issues()[0].Reason, "requires Exec") {
		t.Fatalf("missing actionable malformed-entry issue: %+v", catalog.Issues())
	}
}

func TestDesktopCatalogParsesRepresentativeFlatpakMetadataAndIndexes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "flatpak", "exports", "share", "applications")
	contents := "[Desktop Entry]\r\n" +
		"Type=Application\r\n" +
		"Name=Slack\\sDesktop\r\n" +
		"Name[de]=Slack Deutsch\r\n" +
		"Icon=com.slack.Slack\r\n" +
		"Exec=/usr/bin/flatpak run com.slack.Slack %U\r\n" +
		"TryExec=/usr/bin/flatpak\r\n" +
		"StartupWMClass=Slack\r\n" +
		"X-Flatpak=com.slack.Slack\r\n" +
		"NoDisplay=true\r\n" +
		"Terminal=false\r\n" +
		"DBusActivatable=false\r\n" +
		"SingleMainWindow=true\r\n" +
		"\r\n[Desktop Action NewWindow]\r\nName=New Window\r\nExec=ignored --new-window\r\n"
	path := filepath.Join(root, "com.slack.Slack.desktop")
	writeDesktopTestFile(t, path, contents)

	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{
		Path: root, Origin: DesktopEntryUser, FlatpakInstallation: FlatpakUser,
	}})
	if err != nil {
		t.Fatalf("load Flatpak catalog: %v", err)
	}
	entry, exists := catalog.ByID("com.slack.Slack.desktop")
	if !exists {
		t.Fatal("Flatpak desktop entry missing")
	}
	if entry.Name != "Slack Desktop" || entry.Exec != "/usr/bin/flatpak run com.slack.Slack %U" ||
		entry.TryExec != "/usr/bin/flatpak" || entry.StartupWMClass != "Slack" ||
		entry.FlatpakID != "com.slack.Slack" || entry.FlatpakInstallation != FlatpakUser ||
		!entry.NoDisplay || entry.Terminal || entry.DBusActivatable || !entry.SingleMainWindow {
		t.Fatalf("unexpected parsed Flatpak entry: %+v", entry)
	}
	if got := catalog.ByStartupWMClass("Slack"); len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("StartupWMClass index mismatch: %+v", got)
	}
	if got := catalog.ByFlatpakID("com.slack.Slack"); len(got) != 1 || got[0].ID != entry.ID {
		t.Fatalf("Flatpak index mismatch: %+v", got)
	}
}

func TestDesktopCatalogAcceptsWhitespaceAroundEquals(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	writeDesktopTestFile(t, filepath.Join(root, "org.example.App.desktop"),
		"[Desktop Entry]\nType = Application\nName = Spaced App\nExec = example --flag\n")
	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{Path: root, Origin: DesktopEntrySystem}})
	if err != nil {
		t.Fatal(err)
	}
	entry, exists := catalog.ByID("org.example.App.desktop")
	if !exists || entry.Name != "Spaced App" || entry.Exec != "example --flag" {
		t.Fatalf("valid spaced desktop entry was not parsed: %+v exists=%t issues=%+v", entry, exists, catalog.Issues())
	}
}

func TestDesktopCatalogRejectsCollidingNestedDesktopIDs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	writeDesktopTestFile(t, filepath.Join(root, "a-b.desktop"), desktopTestEntry("Flat", "flat", ""))
	writeDesktopTestFile(t, filepath.Join(root, "a", "b.desktop"), desktopTestEntry("Nested", "nested", ""))
	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{Path: root, Origin: DesktopEntryUser}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := catalog.ByID("a-b.desktop"); exists {
		t.Fatal("colliding desktop file ID was selected arbitrarily")
	}
	if len(catalog.Issues()) != 2 {
		t.Fatalf("expected both collision paths to be diagnosed: %+v", catalog.Issues())
	}
}

func TestDesktopCatalogCacheRequiresExplicitInvalidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	path := filepath.Join(root, "app.desktop")
	writeDesktopTestFile(t, path, desktopTestEntry("Before", "app", ""))
	cache := NewDesktopCatalogCache([]DesktopSearchDirectory{{Path: root, Origin: DesktopEntryUser}})

	first, err := cache.Load()
	if err != nil {
		t.Fatal(err)
	}
	writeDesktopTestFile(t, path, desktopTestEntry("After", "app", ""))
	stillCached, err := cache.Load()
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := stillCached.ByID("app.desktop"); entry.Name != "Before" {
		t.Fatalf("catalog changed without invalidation: %+v", entry)
	}
	cache.Invalidate()
	reloaded, err := cache.Load()
	if err != nil {
		t.Fatal(err)
	}
	if entry, _ := reloaded.ByID("app.desktop"); entry.Name != "After" {
		t.Fatalf("invalidated catalog did not reload: %+v", entry)
	}
	entries := first.Entries()
	entries[0].Name = "mutated copy"
	if entry, _ := first.ByID("app.desktop"); entry.Name != "Before" {
		t.Fatal("caller mutated cached catalog through Entries result")
	}
}

func TestDesktopCatalogBoundsAndRejectsInvalidMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	writeDesktopTestFile(t, filepath.Join(root, "invalid.desktop"), "[Desktop Entry]\nType=Application\nName=Bad\\qEscape\nExec=bad\n")
	writeDesktopTestFile(t, filepath.Join(root, "oversized.desktop"), "[Desktop Entry]\nType=Application\nName=Large\nExec=large\n#"+strings.Repeat("x", MaxDesktopEntrySize))
	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{Path: root, Origin: DesktopEntryUser}})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries()) != 0 || len(catalog.Issues()) != 2 {
		t.Fatalf("invalid desktop entries were not isolated: entries=%+v issues=%+v", catalog.Entries(), catalog.Issues())
	}
}

func TestDesktopCatalogRejectsFIFOWithoutBlocking(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "blocked.desktop"), 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{Path: root, Origin: DesktopEntryUser}})
		if err == nil && len(catalog.Issues()) != 1 {
			err = os.ErrInvalid
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("load catalog with FIFO: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desktop catalog blocked opening a FIFO")
	}
}

func TestDesktopCatalogFollowsSearchRootSymlinkOnly(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-applications")
	linkedRoot := filepath.Join(base, "applications")
	writeDesktopTestFile(t, filepath.Join(realRoot, "org.example.App.desktop"), desktopTestEntry("Linked Root", "example", ""))
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	catalog, err := LoadDesktopCatalog([]DesktopSearchDirectory{{Path: linkedRoot, Origin: DesktopEntryUser}})
	if err != nil {
		t.Fatal(err)
	}
	entry, exists := catalog.ByID("org.example.App.desktop")
	if !exists || entry.Name != "Linked Root" || entry.Path != filepath.Join(linkedRoot, "org.example.App.desktop") {
		t.Fatalf("symlinked XDG applications root was not traversed safely: %+v exists=%t issues=%+v", entry, exists, catalog.Issues())
	}
}

func TestDesktopFilesEnforcesRemainingGlobalFilesystemBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ordinary-file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := desktopFiles(root, 1); err == nil || !strings.Contains(err.Error(), "filesystem entries") {
		t.Fatalf("expected exhausted global filesystem budget, got %v", err)
	}
}

func TestDesktopCatalogSharesFilesystemBudgetAcrossSearchRoots(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "ordinary-file"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := loadDesktopCatalog([]DesktopSearchDirectory{
		{Path: first, Origin: DesktopEntryUser},
		{Path: second, Origin: DesktopEntryUser},
	}, 3)
	if err == nil || !strings.Contains(err.Error(), "maximum 3 filesystem entries") {
		t.Fatalf("expected one cumulative catalog budget, got %v", err)
	}
}

func desktopTestEntry(name string, executable string, extra string) string {
	return "[Desktop Entry]\nType=Application\nName=" + name + "\nExec=" + executable + "\n" + extra
}

func writeDesktopTestFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
