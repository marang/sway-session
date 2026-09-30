package buildmetadata

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

const testCommit = "1234567890123456789012345678901234567890"

func testStamp(version string) string {
	return stampPrefix + version + "|" + testCommit + "|true" + stampSuffix
}

func TestReadRecordedMetadata(t *testing.T) {
	for _, flags := range []string{"-s -X main.version=1.2.3 -X main.commit=" + testCommit + " -X main.modified=true", "-X 'main.version=1.2.3' -X=main.commit=" + testCommit + " -X main.modified=true"} {
		value := Read(&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-ldflags", Value: flags}}})
		if value != (Metadata{Version: "1.2.3", Commit: testCommit, Modified: true}) {
			t.Fatalf("%q: %+v", flags, value)
		}
	}
	fallback := &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: testCommit}, {Key: "-ldflags", Value: "-X 'unterminated"}}}
	if got := Read(fallback); got.Version != "v1.0.0" || got.Commit != testCommit {
		t.Fatalf("fallback: %+v", got)
	}
	if Read(nil) != unknown() {
		t.Fatal("nil information must be unknown")
	}
	if Read(&debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}) != unknown() {
		t.Fatal("legacy devel must be unknown")
	}
	if got := Read(&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-ldflags", Value: "-X main.version=$(bad) -X main.commit=bad"}}}); got != unknown() {
		t.Fatalf("unsafe values: %+v", got)
	}
}

func TestStampBoundariesAmbiguityAndCancellation(t *testing.T) {
	stamp := testStamp("1.2.3")
	data := strings.Repeat("x", (64<<10)-10) + stamp + stamp
	value, found, err := readStamp(context.Background(), strings.NewReader(data))
	if err != nil || !found || value.Version != "1.2.3" {
		t.Fatalf("boundary: %+v %v %v", value, found, err)
	}
	if _, _, err = readStamp(context.Background(), strings.NewReader(stamp+testStamp("2.0.0"))); err == nil {
		t.Fatal("conflicting stamps accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = readStamp(ctx, bytes.NewReader(nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, malformed := range []string{testStamp("bad version"), strings.Replace(stamp, testCommit, "bad", 1), strings.Replace(stamp, "|true|", "|maybe|", 1)} {
		if _, ok := parseStamp(malformed); ok {
			t.Fatalf("malformed accepted: %q", malformed)
		}
	}
}

// Inspect actual production flags, including Arch's PIE. Never execute the
// artifact: reading metadata must remain independent of its entry point.
func TestReadStrippedTrimpathPIEExecutable(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "sway-session")
	flags := "-s -w -buildid= -X main.version=9.8.7 -X github.com/marang/sway-session/internal/buildmetadata.Stamp=" + testStamp("9.8.7")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-buildmode=pie", "-ldflags", flags, "-o", artifact, "./cmd/sway-session")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// A stripped legacy binary with only main.version cannot expose that flag
	// under trimpath. It must report unknown rather than invent a product ID.
	legacy := filepath.Join(t.TempDir(), "legacy")
	legacyBuild := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w -buildid= -X main.version=0.4.3", "-o", legacy, "./cmd/sway-session")
	legacyBuild.Dir = root
	legacyBuild.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := legacyBuild.CombinedOutput(); err != nil {
		t.Fatalf("legacy build: %v\n%s", err, output)
	}
	if got, err := ReadFile(legacy); err != nil || got != unknown() {
		t.Fatalf("legacy metadata: %+v %v", got, err)
	}
	info, err := buildinfo.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "-ldflags" {
			t.Fatal("fixture must exercise trimpath's missing recorded linker flags")
		}
	}
	file, err := os.Open(artifact)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err = file.Seek(7, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	value, err := ReadExecutable(context.Background(), file)
	if err != nil || value != (Metadata{Version: "9.8.7", Commit: testCommit, Modified: true}) {
		t.Fatalf("metadata: %+v %v", value, err)
	}
	offset, _ := file.Seek(0, io.SeekCurrent)
	if offset != 7 {
		t.Fatalf("seek position changed: %d", offset)
	}
	// Pinning the descriptor survives replacement/removal of its pathname.
	if err = os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if pinned, err := ReadExecutable(context.Background(), file); err != nil || pinned != value {
		t.Fatalf("pinned metadata: %+v %v", pinned, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadExecutable(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestReadExecutableRejectsNonRegularAndOversized(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "large")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = file.Truncate(maxExecutableSize + 1); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadExecutable(context.Background(), file); err == nil {
		t.Fatal("oversized executable accepted")
	}
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if _, err = ReadExecutable(context.Background(), directory); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err = ReadExecutable(context.Background(), nil); err == nil {
		t.Fatal("nil descriptor accepted")
	}
}

func TestExecutingLegacyVersionOverride(t *testing.T) {
	if Stamp != "" {
		t.Skip("test binary stamped")
	}
	value := Executing("0.1.2", testCommit, "true")
	if value != (Metadata{Version: "0.1.2", Commit: testCommit, Modified: true}) {
		t.Fatalf("legacy main linker metadata: %+v", value)
	}
	if got := Current(); got.Version == "unknown" {
		t.Fatalf("unstamped executing build should identify development: %+v", got)
	}
}
