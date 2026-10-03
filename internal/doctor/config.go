package doctor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	swayIntegrationFixID = "sway.integration"
	maxSwayConfigBytes   = 1 << 20
	maxSwayConfigTotal   = 4 << 20
	maxSwayConfigFiles   = 64
	maxSwayIncludeDepth  = 16
	maxSwayBlockDepth    = 64
	maxSwayConfigLine    = 64 << 10
	maxSwayEvidenceItems = 8
)

type integrationKind uint8

const (
	integrationDaemon integrationKind = iota
	integrationRestore
	integrationPersistent
	integrationEphemeral
)

var integrationOrder = []integrationKind{
	integrationDaemon,
	integrationRestore,
	integrationPersistent,
	integrationEphemeral,
}

type configLocation struct {
	path string
	line int
}

type integrationOccurrence struct {
	kind    integrationKind
	correct bool
	where   configLocation
}

type swayConfigAnalysis struct {
	root string
	live bool
	swayConfigEvidence
	includes map[string]struct{}
	files    int
	observed []configFingerprint
}

type configUncertainty struct {
	where  configLocation
	reason string
}

type configFingerprint struct {
	path   string
	state  safeFileState
	digest [sha256.Size]byte
}

func inspectSwayConfig(ctx context.Context, options Options) []Check {
	analysis, err := analyzeSwayConfig(ctx, options)
	if err != nil {
		evidence := make([]string, 0, len(integrationOrder))
		for _, kind := range integrationOrder {
			evidence = append(evidence, fmt.Sprintf("%s: could not be fully checked", integrationLabel(kind)))
		}
		return []Check{{
			ID:       swayIntegrationFixID,
			Title:    "Sway integration",
			Status:   Unavailable,
			Detail:   "The selected Sway configuration could not be inspected safely.",
			Hint:     err.Error(),
			Evidence: evidence,
		}}
	}

	check := Check{ID: swayIntegrationFixID, Title: "Sway integration"}
	source := "on-disk Sway configuration"
	if analysis.live {
		source = "configuration path reported by the active Sway compositor"
	}
	check.Evidence = append(check.Evidence, fmt.Sprintf("statically inspected %s: %s", source, analysis.root))
	if analysis.files > 1 {
		check.Evidence = append(check.Evidence, fmt.Sprintf("followed %d safe configuration files", analysis.files))
	}

	missing := make([]string, 0, len(integrationOrder))
	conflicts := make([]string, 0, len(integrationOrder))
	unknown := make([]string, 0, len(integrationOrder))
	concluded := 0
	haveEvidence := false
	for _, kind := range integrationOrder {
		occurrences := analysis.occurrences[kind]
		occurrenceCount := analysis.occurrenceCounts[kind]
		correct := analysis.matchingCounts[kind]
		haveEvidence = haveEvidence || occurrenceCount != 0
		conflicting := occurrenceCount > 1 || (occurrenceCount == 1 && correct != 1)
		if conflicting {
			concluded++
			conflicts = append(conflicts, integrationLabel(kind))
			check.Evidence = append(check.Evidence, fmt.Sprintf("%s: conflicting", integrationLabel(kind)))
			for _, occurrence := range occurrences {
				state := "conflicting declaration"
				if occurrence.correct {
					state = "matching declaration"
				}
				check.Evidence = append(check.Evidence,
					fmt.Sprintf("%s: %s at %s:%d", integrationLabel(kind), state, occurrence.where.path, occurrence.where.line))
			}
			if omitted := occurrenceCount - len(occurrences); omitted > 0 {
				check.Evidence = append(check.Evidence,
					fmt.Sprintf("%s: %d additional declarations omitted", integrationLabel(kind), omitted))
			}
			if analysis.uncertaintyCounts[kind] == 0 {
				continue
			}
		}
		if analysis.uncertaintyCounts[kind] != 0 {
			unknown = append(unknown, integrationLabel(kind))
			evidence := fmt.Sprintf("%s: could not be fully checked", integrationLabel(kind))
			if occurrenceCount == 1 && len(occurrences) == 1 && occurrences[0].correct {
				evidence += fmt.Sprintf("; matching declaration observed at %s:%d", occurrences[0].where.path, occurrences[0].where.line)
			}
			check.Evidence = append(check.Evidence, evidence)
			limitations := analysis.uncertain[kind]
			for _, limitation := range limitations {
				evidence := fmt.Sprintf("%s: %s", integrationLabel(kind), limitation.reason)
				if limitation.where.path != "" && !strings.Contains(limitation.reason, limitation.where.path) {
					evidence += fmt.Sprintf(" at %s:%d", limitation.where.path, limitation.where.line)
				}
				check.Evidence = append(check.Evidence, evidence)
			}
			if analysis.uncertaintySaturated[kind] {
				check.Evidence = append(check.Evidence,
					fmt.Sprintf("%s: additional limitations omitted after the diagnostic deduplication limit", integrationLabel(kind)))
			} else if omitted := analysis.uncertaintyCounts[kind] - len(limitations); omitted > 0 {
				check.Evidence = append(check.Evidence,
					fmt.Sprintf("%s: %d additional limitations omitted", integrationLabel(kind), omitted))
			}
			continue
		}
		concluded++
		switch {
		case occurrenceCount == 0:
			missing = append(missing, integrationLabel(kind))
			check.Evidence = append(check.Evidence, fmt.Sprintf("%s: missing", integrationLabel(kind)))
		default:
			where := occurrences[0].where
			check.Evidence = append(check.Evidence,
				fmt.Sprintf("%s: present at %s:%d", integrationLabel(kind), where.path, where.line))
		}
	}

	if len(unknown) != 0 {
		check.Hint = "Automatic repair is unavailable because the uncertain declarations require manual review."
		if analysis.unsupported != nil {
			check.Hint = "Automatic repair is unavailable: " + analysis.unsupported.Error()
		}
		if concluded == 0 && !haveEvidence {
			check.Status = Unavailable
			check.Detail = "The Sway integration requirements could not be checked safely."
		} else {
			check.Status = Warning
			check.Detail = "The Sway integration was partially checked; some requirements remain uncertain."
		}
		return []Check{check}
	}

	if len(conflicts) != 0 {
		check.Status = Warning
		check.Detail = "Relevant Sway startup or shortcut declarations are duplicated or conflicting."
		check.Hint = "Review these declarations manually; doctor will not replace or reorder user shortcuts."
		return []Check{check}
	}
	if len(missing) != 0 {
		check.Status = Warning
		check.Detail = "The configuration file is missing: " + strings.Join(missing, ", ") + "."
		check.Hint = "This is a static file check; apply the repair, then reload Sway when convenient."
		if _, safetyErr := New(options).Plan(ctx, swayIntegrationFixID); safetyErr == nil {
			check.FixID = swayIntegrationFixID
		} else {
			check.Hint = "Automatic repair is unavailable: " + safetyErr.Error()
		}
		return []Check{check}
	}

	check.Status = OK
	check.Detail = "The configuration files contain the one-time startups and default persistent and ephemeral shortcuts."
	check.Hint = "This static check does not claim that any particular binding is active in the running compositor."
	return []Check{check}
}

func analyzeSwayConfig(ctx context.Context, options Options) (swayConfigAnalysis, error) {
	return analyzeSwayConfigWithEdits(ctx, options, nil)
}

func analyzeSwayConfigWithEdits(ctx context.Context, options Options, edits []fileEdit) (swayConfigAnalysis, error) {
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
	analysis := swayConfigAnalysis{
		root:               selection.path,
		live:               selection.live,
		swayConfigEvidence: newSwayConfigEvidence(),
		includes:           make(map[string]struct{}),
	}
	scanner := swayStaticScanner{
		ctx:        ctx,
		analysis:   &analysis,
		classifier: newSwayConfigClassifier(defaultSwayVariables()),
		active:     make(map[string]bool),
		visited:    make(map[string]bool),
		overrides:  make(map[string][]byte),
	}
	for _, edit := range edits {
		scanner.overrides[edit.path] = edit.newContent
	}
	if err := scanner.scan(selection.path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return swayConfigAnalysis{}, fmt.Errorf("sway configuration %s does not exist; create it manually before using repair", selection.path)
		}
		return swayConfigAnalysis{}, err
	}
	return analysis, nil
}

type swayStaticScanner struct {
	ctx        context.Context
	analysis   *swayConfigAnalysis
	classifier *swayConfigClassifier
	active     map[string]bool
	visited    map[string]bool
	overrides  map[string][]byte
	total      int64
	exhausted  bool
}

func (scanner *swayStaticScanner) scan(path string, depth int) error {
	if err := scanner.ctx.Err(); err != nil {
		return err
	}
	if scanner.exhausted {
		return nil
	}
	if depth > maxSwayIncludeDepth {
		scanner.analysis.markUncertainAll(configLocation{path: path},
			fmt.Sprintf("include nesting exceeds the supported depth of %d", maxSwayIncludeDepth))
		return nil
	}
	clean, err := cleanAbsolutePath(path)
	if err != nil {
		scanner.analysis.markUncertainAll(configLocation{path: path}, fmt.Sprintf("unsafe include path %q: %v", path, err))
		return nil
	}
	if scanner.active[clean] {
		scanner.analysis.markUncertainAll(configLocation{path: clean}, fmt.Sprintf("include cycle detected at %s", clean))
		return nil
	}
	if scanner.visited[clean] {
		return nil
	}
	if scanner.analysis.files >= maxSwayConfigFiles {
		scanner.exhausted = true
		scanner.analysis.markUncertainAll(configLocation{path: clean},
			fmt.Sprintf("include graph exceeds the supported limit of %d files", maxSwayConfigFiles))
		return nil
	}

	content, overridden := scanner.overrides[clean]
	var state safeFileState
	if !overridden {
		content, state, err = readSafeConfigFile(clean)
		if err != nil {
			if depth > 0 {
				scanner.analysis.markUncertainAll(configLocation{path: clean},
					fmt.Sprintf("included Sway configuration %s could not be inspected safely", clean))
				return nil
			}
			return fmt.Errorf("inspect Sway configuration %s: %w", clean, err)
		}
	}
	scanner.analysis.observed = append(scanner.analysis.observed, configFingerprint{path: clean, state: state, digest: sha256.Sum256(content)})
	scanner.total += int64(len(content))
	if scanner.total > maxSwayConfigTotal {
		scanner.exhausted = true
		scanner.analysis.markUncertainAll(configLocation{path: clean}, "include graph exceeds the supported byte limit")
		return nil
	}
	scanner.analysis.files++
	scanner.visited[clean] = true
	scanner.active[clean] = true
	defer delete(scanner.active, clean)

	parser := scanner.classifier.source(clean)
	var traversalErr error
	err = forEachSwayLogicalLine(content, func(logical swayLogicalLine) bool {
		if scanner.ctx.Err() != nil {
			return false
		}
		facts := parser.line(logical)
		scanner.analysis.record(facts)
		if len(facts.includes) != 0 {
			traversalErr = scanner.followIncludes(clean, logical.start, facts.includes, depth)
			if traversalErr != nil {
				return false
			}
		}
		return true
	})
	if err != nil {
		scanner.analysis.markUncertainAll(configLocation{path: clean}, "cannot safely parse configuration: "+err.Error())
	}
	if traversalErr != nil {
		return traversalErr
	}
	if err := scanner.ctx.Err(); err != nil {
		return err
	}
	scanner.analysis.record(parser.finish())

	return nil
}

func (scanner *swayStaticScanner) followIncludes(parent string, line int, patterns []string, depth int) error {
	if scanner.exhausted {
		return nil
	}
	location := configLocation{path: parent, line: line}
	if len(patterns) == 0 {
		scanner.analysis.markUncertainAll(location, fmt.Sprintf("include at %s:%d has no path", parent, line))
		return nil
	}
	for _, pattern := range patterns {
		expanded := pattern
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(filepath.Dir(parent), expanded)
		}
		matches, err := filepath.Glob(expanded)
		if err != nil {
			scanner.analysis.markUncertainAll(location, fmt.Sprintf("invalid include glob at %s:%d", parent, line))
			continue
		}
		for path := range scanner.overrides {
			if matched, _ := filepath.Match(expanded, path); matched && !containsPath(matches, path) {
				matches = append(matches, path)
			}
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			clean := filepath.Clean(expanded)
			managed := filepath.Join(filepath.Dir(scanner.analysis.root), doctorSnippetName)
			if !strings.ContainsAny(expanded, "*?[") && clean == managed {
				scanner.analysis.includes[clean] = struct{}{}
				continue
			}
			// Sway treats an include glob with no matches as a no-op. This is a
			// common way to make optional config fragments available.
			if strings.ContainsAny(expanded, "*?[") {
				continue
			}
			scanner.analysis.markUncertainAll(location,
				fmt.Sprintf("include at %s:%d does not match an existing file", parent, line))
			continue
		}
		if len(matches)+scanner.analysis.files > maxSwayConfigFiles {
			scanner.exhausted = true
			scanner.analysis.markUncertainAll(location,
				fmt.Sprintf("include graph exceeds the supported limit of %d files", maxSwayConfigFiles))
			return nil
		}
		for _, match := range matches {
			clean := filepath.Clean(match)
			scanner.analysis.includes[clean] = struct{}{}
			if err := scanner.scan(clean, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsPath(paths []string, path string) bool {
	for _, candidate := range paths {
		if candidate == path {
			return true
		}
	}
	return false
}

func defaultSwayVariables() map[string]string {
	variables := make(map[string]string)
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		variables["$HOME"] = home
	}
	if config := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(config) {
		variables["$XDG_CONFIG_HOME"] = filepath.Clean(config)
	}
	return variables
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
