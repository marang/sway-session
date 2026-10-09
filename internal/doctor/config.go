package doctor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	swayIntegrationFixID = "sway.integration"
	maxSwayConfigBytes   = 1 << 20
	maxSwayConfigLine    = 64 << 10
	maxSwayBlockDepth    = 64
)

type integrationKind uint8

const (
	integrationDaemon integrationKind = iota
	integrationRestore
	integrationPersistent
	integrationEphemeral
)

var integrationOrder = []integrationKind{
	integrationDaemon, integrationRestore, integrationPersistent, integrationEphemeral,
}

type managedSnippet struct {
	executable string
	shortcuts  ShortcutSelection
	legacy     bool
}

// The selected main file and its one standard sibling are the entire source
// inspection boundary. Other includes and command bodies are never read.
type swayConfigAnalysis struct {
	root           string
	live           bool
	rootContent    []byte
	rootState      safeFileState
	snippetPath    string
	snippetContent []byte
	snippetState   safeFileState
	snippetExists  bool
	snippet        managedSnippet
	snippetErr     error
	includeLine    int
	observed       []configFingerprint
}

type configFingerprint struct {
	path   string
	state  safeFileState
	digest [sha256.Size]byte
}

func inspectSwayConfig(ctx context.Context, options Options) []Check {
	check := Check{ID: swayIntegrationFixID, Title: "Sway integration"}
	analysis, err := analyzeSwayConfig(ctx, options)
	if err != nil {
		check.Status = Unavailable
		check.Detail = "The selected main file and standard Sway integration could not be inspected safely."
		check.Hint = err.Error()
		return []Check{check}
	}
	source := "on-disk Sway configuration"
	if analysis.live {
		source = "configuration path reported by the active Sway compositor"
	}
	check.Evidence = append(check.Evidence,
		fmt.Sprintf("selected main file (%s): %s", source, analysis.root),
		"standard file: "+analysis.snippetPath)
	if analysis.includeLine != 0 {
		check.Evidence = append(check.Evidence,
			fmt.Sprintf("literal direct include: present at %s:%d", analysis.root, analysis.includeLine))
	} else {
		check.Evidence = append(check.Evidence, "literal direct include: not observed in the selected main file")
	}
	check.Hint = "This checks source files only; it does not establish effective load order or live binding activity."
	switch {
	case analysis.snippetErr != nil:
		check.Status = Unavailable
		check.Detail = "The standard file is unrecognized or manually edited and is protected."
		check.Hint = "Move or remove this file manually before adopting the standard integration: " + analysis.snippetErr.Error()
	case analysis.snippetExists && analysis.snippet.legacy:
		check.Status = Warning
		check.Detail = "The standard file contains a legacy partial profile that requires manual migration."
		check.Evidence = append(check.Evidence, "standard profile: legacy partial")
		check.Hint = "Move or remove this legacy file manually before adopting a supported standard profile. Review previous integration and load order yourself."
	case !analysis.snippetExists:
		check.Status = Unavailable
		if analysis.includeLine != 0 {
			check.Status = Warning
		}
		check.Detail = "The standard integration file is missing."
		check.Evidence = append(check.Evidence, "standard file: missing")
		check.Hint = "Explicitly adopt the standard integration to create or recover this file. Review previous integration and load order yourself."
	case analysis.includeLine == 0:
		check.Status = Warning
		check.Detail = "A supported standard file exists, but a literal direct include was not observed in the selected main file."
		check.Evidence = append(check.Evidence, standardProfileEvidence(analysis.snippet.shortcuts))
		check.Hint += " Explicit adoption can append a direct include; review previous integration and load order yourself."
	default:
		check.Status = OK
		check.Detail = "The standard file contains a supported profile and the selected main file contains its literal direct include."
		check.Evidence = append(check.Evidence, standardProfileEvidence(analysis.snippet.shortcuts))
	}
	if analysis.snippetErr == nil && !analysis.snippet.legacy {
		if _, err := repairDirectory(analysis); err == nil {
			check.FixID = swayIntegrationFixID
			check.AdoptionRequired = !analysis.snippetExists || analysis.includeLine == 0
		}
	}
	return []Check{check}
}

func standardProfileEvidence(shortcuts ShortcutSelection) string {
	if shortcuts == ShortcutsDefault {
		return "standard profile: default shortcuts"
	}
	return "standard profile: startup-only"
}

func analyzeSwayConfig(ctx context.Context, options Options) (swayConfigAnalysis, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return swayConfigAnalysis{}, err
	}
	selection, err := resolveSwayConfigPathSelection(ctx, options)
	if err != nil {
		return swayConfigAnalysis{}, fmt.Errorf("resolve Sway configuration: %w", err)
	}
	analysis := swayConfigAnalysis{root: selection.path, live: selection.live,
		snippetPath: filepath.Join(filepath.Dir(selection.path), doctorSnippetName)}
	if analysis.root == analysis.snippetPath {
		return swayConfigAnalysis{}, errors.New("selected main file cannot be the doctor-managed snippet")
	}
	analysis.rootContent, analysis.rootState, err = readSafeConfigFile(analysis.root)
	if err != nil {
		return swayConfigAnalysis{}, fmt.Errorf("inspect selected main Sway configuration %s: %w; create an absent main file manually", analysis.root, err)
	}
	analysis.observed = append(analysis.observed, configFingerprint{
		path: analysis.root, state: analysis.rootState, digest: sha256.Sum256(analysis.rootContent)})
	analysis.includeLine = literalDirectInclude(analysis.rootContent, analysis.root, analysis.snippetPath)
	analysis.snippetContent, analysis.snippetState, analysis.snippetExists, err = readOptionalRepairFile(analysis.snippetPath)
	if err != nil {
		return swayConfigAnalysis{}, fmt.Errorf("inspect standard integration file %s: %w", analysis.snippetPath, err)
	}
	if analysis.snippetExists {
		analysis.observed = append(analysis.observed, configFingerprint{path: analysis.snippetPath,
			state: analysis.snippetState, digest: sha256.Sum256(analysis.snippetContent)})
		analysis.snippet, analysis.snippetErr = parseManagedSnippet(analysis.snippetContent)
	}
	if err := ctx.Err(); err != nil {
		return swayConfigAnalysis{}, err
	}
	return analysis, nil
}

func repairDirectory(analysis swayConfigAnalysis) (safeDirectoryState, error) {
	if analysis.rootState.owner != uint32(os.Getuid()) {
		return safeDirectoryState{}, errors.New("repair requires the selected main file to be owned by the current user")
	}
	if analysis.snippetExists && analysis.snippetState.owner != uint32(os.Getuid()) {
		return safeDirectoryState{}, errors.New("doctor-managed snippet is not owned by the current user")
	}
	return inspectSafeRepairDirectory(filepath.Dir(analysis.root))
}

type safeFileState struct {
	device uint64
	inode  uint64
	mode   os.FileMode
	size   int64
	mtime  int64
	owner  uint32
}

func readSafeConfigFile(path string) ([]byte, safeFileState, error) {
	return readSafeConfigFileAt(unix.AT_FDCWD, path)
}

func readSafeConfigFileAt(directory int, path string) ([]byte, safeFileState, error) {
	fd, err := unix.Openat2(directory, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, safeFileState{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, safeFileState{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, safeFileState{}, errors.New("must be a regular file")
	}
	if stat.Nlink != 1 {
		return nil, safeFileState{}, fmt.Errorf("link count is %d; expected 1", stat.Nlink)
	}
	if stat.Mode&0o022 != 0 {
		return nil, safeFileState{}, errors.New("must not be group- or world-writable")
	}
	uid := uint32(os.Getuid())
	if stat.Uid != uid && stat.Uid != 0 {
		return nil, safeFileState{}, fmt.Errorf("owner uid %d is neither the current user nor root", stat.Uid)
	}
	if stat.Size > maxSwayConfigBytes {
		return nil, safeFileState{}, fmt.Errorf("file exceeds the supported size of %d bytes", maxSwayConfigBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxSwayConfigBytes+1))
	if err != nil {
		return nil, safeFileState{}, err
	}
	if len(content) > maxSwayConfigBytes {
		return nil, safeFileState{}, fmt.Errorf("file exceeds the supported size of %d bytes", maxSwayConfigBytes)
	}
	state := safeFileState{
		device: uint64(stat.Dev), inode: stat.Ino, mode: os.FileMode(stat.Mode & 0o777),
		size: stat.Size, mtime: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec, owner: stat.Uid,
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, safeFileState{}, err
	}
	afterState := safeFileState{
		device: uint64(after.Dev), inode: after.Ino, mode: os.FileMode(after.Mode & 0o777),
		size: after.Size, mtime: after.Mtim.Sec*1e9 + after.Mtim.Nsec, owner: after.Uid,
	}
	if afterState != state || int64(len(content)) != state.size {
		return nil, safeFileState{}, errors.New("file changed while it was inspected")
	}
	return content, state, nil
}

func cleanAbsolutePath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
		return "", errors.New("path must be a clean absolute path")
	}
	return path, nil
}
