package doctor

import (
	"bufio"
	"context"
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
	maxSwayConfigLine    = 64 << 10
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
	root        string
	live        bool
	occurrences map[integrationKind][]integrationOccurrence
	includes    map[string]struct{}
	files       int
	unsupported error
}

func inspectSwayConfig(ctx context.Context, options Options) []Check {
	analysis, err := analyzeSwayConfig(ctx, options)
	if err != nil {
		return []Check{{
			ID:     swayIntegrationFixID,
			Title:  "Sway integration",
			Status: Unavailable,
			Detail: "The selected Sway configuration could not be inspected safely.",
			Hint:   err.Error(),
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

	if analysis.unsupported != nil {
		check.Status = Unavailable
		check.Detail = "The Sway configuration uses syntax or paths this static check cannot safely interpret."
		check.Hint = analysis.unsupported.Error()
		return []Check{check}
	}

	missing := make([]string, 0, len(integrationOrder))
	conflicts := make([]string, 0, len(integrationOrder))
	for _, kind := range integrationOrder {
		occurrences := analysis.occurrences[kind]
		correct := 0
		for _, occurrence := range occurrences {
			if occurrence.correct {
				correct++
			}
		}
		switch {
		case len(occurrences) == 0:
			missing = append(missing, integrationLabel(kind))
		case len(occurrences) != 1 || correct != 1:
			conflicts = append(conflicts, integrationLabel(kind))
			for _, occurrence := range occurrences {
				state := "conflicting declaration"
				if occurrence.correct {
					state = "matching declaration"
				}
				check.Evidence = append(check.Evidence,
					fmt.Sprintf("%s: %s at %s:%d", integrationLabel(kind), state, occurrence.where.path, occurrence.where.line))
			}
		default:
			where := occurrences[0].where
			check.Evidence = append(check.Evidence,
				fmt.Sprintf("%s: present at %s:%d", integrationLabel(kind), where.path, where.line))
		}
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
		if safetyErr := repairSafetyAvailable(analysis, options); safetyErr == nil {
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
		root:        selection.path,
		live:        selection.live,
		occurrences: make(map[integrationKind][]integrationOccurrence),
		includes:    make(map[string]struct{}),
	}
	variables := defaultSwayVariables()
	scanner := swayStaticScanner{
		ctx:       ctx,
		analysis:  &analysis,
		variables: variables,
		active:    make(map[string]bool),
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
	ctx       context.Context
	analysis  *swayConfigAnalysis
	variables map[string]string
	active    map[string]bool
	total     int64
}

func (scanner *swayStaticScanner) scan(path string, depth int) error {
	if err := scanner.ctx.Err(); err != nil {
		return err
	}
	if depth > maxSwayIncludeDepth {
		scanner.analysis.unsupported = fmt.Errorf("include nesting exceeds the supported depth of %d", maxSwayIncludeDepth)
		return nil
	}
	clean, err := cleanAbsolutePath(path)
	if err != nil {
		scanner.analysis.unsupported = fmt.Errorf("unsafe include path %q: %w", path, err)
		return nil
	}
	if scanner.active[clean] {
		scanner.analysis.unsupported = fmt.Errorf("include cycle detected at %s", clean)
		return nil
	}
	if scanner.analysis.files >= maxSwayConfigFiles {
		scanner.analysis.unsupported = fmt.Errorf("include graph exceeds the supported limit of %d files", maxSwayConfigFiles)
		return nil
	}

	content, _, err := readSafeConfigFile(clean)
	if err != nil {
		return fmt.Errorf("inspect Sway configuration %s: %w", clean, err)
	}
	scanner.total += int64(len(content))
	if scanner.total > maxSwayConfigTotal {
		scanner.analysis.unsupported = fmt.Errorf("include graph exceeds the supported byte limit")
		return nil
	}
	scanner.analysis.files++
	scanner.active[clean] = true
	defer delete(scanner.active, clean)

	lineScanner := bufio.NewScanner(strings.NewReader(string(content)))
	lineScanner.Buffer(make([]byte, 4096), maxSwayConfigLine)
	line := 0
	for lineScanner.Scan() {
		line++
		if err := scanner.ctx.Err(); err != nil {
			return err
		}
		tokens, unsupported, err := tokenizeSwayLine(lineScanner.Text())
		if err != nil {
			scanner.analysis.unsupported = fmt.Errorf("cannot safely parse %s:%d: %w", clean, line, err)
			return nil
		}
		if unsupported {
			scanner.analysis.unsupported = fmt.Errorf("unsupported compound or continued syntax at %s:%d", clean, line)
			return nil
		}
		if len(tokens) == 0 {
			continue
		}
		location := configLocation{path: clean, line: line}
		scanner.recordIntegration(tokens, location)
		switch strings.ToLower(tokens[0]) {
		case "set":
			if len(tokens) >= 3 && strings.HasPrefix(tokens[1], "$") && validSwayVariable(tokens[1]) {
				scanner.variables[tokens[1]] = strings.Join(tokens[2:], " ")
			}
		case "include":
			if err := scanner.followIncludes(clean, line, tokens[1:], depth); err != nil {
				return err
			}
			if scanner.analysis.unsupported != nil {
				return nil
			}
		}
	}
	if err := lineScanner.Err(); err != nil {
		scanner.analysis.unsupported = fmt.Errorf("a line in %s exceeds the supported length", clean)
		return nil
	}
	return nil
}

func (scanner *swayStaticScanner) followIncludes(parent string, line int, patterns []string, depth int) error {
	if len(patterns) == 0 {
		scanner.analysis.unsupported = fmt.Errorf("include at %s:%d has no path", parent, line)
		return nil
	}
	for _, pattern := range patterns {
		expanded, err := expandSwayInclude(pattern, scanner.variables)
		if err != nil {
			scanner.analysis.unsupported = fmt.Errorf("cannot safely expand include at %s:%d: %w", parent, line, err)
			return nil
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(filepath.Dir(parent), expanded)
		}
		matches, err := filepath.Glob(expanded)
		if err != nil {
			scanner.analysis.unsupported = fmt.Errorf("invalid include glob at %s:%d", parent, line)
			return nil
		}
		sort.Strings(matches)
		if len(matches) == 0 {
			clean := filepath.Clean(expanded)
			managed := filepath.Join(filepath.Dir(scanner.analysis.root), doctorSnippetName)
			if !strings.ContainsAny(expanded, "*?[") && clean == managed {
				scanner.analysis.includes[clean] = struct{}{}
				continue
			}
			scanner.analysis.unsupported = fmt.Errorf("include at %s:%d does not match an existing file", parent, line)
			return nil
		}
		if len(matches)+scanner.analysis.files > maxSwayConfigFiles {
			scanner.analysis.unsupported = fmt.Errorf("include graph exceeds the supported limit of %d files", maxSwayConfigFiles)
			return nil
		}
		for _, match := range matches {
			clean := filepath.Clean(match)
			scanner.analysis.includes[clean] = struct{}{}
			if err := scanner.scan(clean, depth+1); err != nil {
				return err
			}
			if scanner.analysis.unsupported != nil {
				return nil
			}
		}
	}
	return nil
}

func (scanner *swayStaticScanner) recordIntegration(tokens []string, location configLocation) {
	if kind, relevant, correct := classifyStartup(tokens); relevant {
		scanner.analysis.occurrences[kind] = append(scanner.analysis.occurrences[kind], integrationOccurrence{
			kind: kind, correct: correct, where: location,
		})
	}
	if kind, relevant, correct := classifyBinding(tokens); relevant {
		scanner.analysis.occurrences[kind] = append(scanner.analysis.occurrences[kind], integrationOccurrence{
			kind: kind, correct: correct, where: location,
		})
	}
}

func classifyStartup(tokens []string) (integrationKind, bool, bool) {
	if len(tokens) < 2 || (tokens[0] != "exec" && tokens[0] != "exec_always") {
		return 0, false, false
	}
	command := skipExecOptions(tokens[1:])
	if len(command) < 2 || filepath.Base(command[0]) != "sway-session" {
		return 0, false, false
	}
	var kind integrationKind
	switch command[1] {
	case "daemon":
		kind = integrationDaemon
	case "restore":
		kind = integrationRestore
	default:
		return 0, false, false
	}
	return kind, true, tokens[0] == "exec" && len(command) == 2
}

func classifyBinding(tokens []string) (integrationKind, bool, bool) {
	if len(tokens) < 3 || tokens[0] != "bindsym" {
		return 0, false, false
	}
	index := 1
	for index < len(tokens) && strings.HasPrefix(tokens[index], "--") {
		index++
	}
	if index >= len(tokens) {
		return 0, false, false
	}
	key := tokens[index]
	var kind integrationKind
	switch key {
	case "$mod+Return":
		kind = integrationPersistent
	case "$mod+Shift+Return":
		kind = integrationEphemeral
	default:
		return 0, false, false
	}
	command := tokens[index+1:]
	if len(command) == 0 || command[0] != "exec" {
		return kind, true, false
	}
	command = skipExecOptions(command[1:])
	if len(command) != 3 || filepath.Base(command[0]) != "sway-session" || command[1] != "terminal" {
		return kind, true, false
	}
	if kind == integrationPersistent {
		return kind, true, command[2] == "--new"
	}
	return kind, true, command[2] == "--ephemeral"
}

func skipExecOptions(tokens []string) []string {
	for len(tokens) > 0 && strings.HasPrefix(tokens[0], "--") {
		tokens = tokens[1:]
	}
	return tokens
}

func integrationLabel(kind integrationKind) string {
	switch kind {
	case integrationDaemon:
		return "one-time daemon startup"
	case integrationRestore:
		return "one-time restore startup"
	case integrationPersistent:
		return "default persistent-terminal shortcut"
	case integrationEphemeral:
		return "default ephemeral-terminal shortcut"
	default:
		return "unknown integration"
	}
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

func validSwayVariable(variable string) bool {
	if len(variable) < 2 || variable[0] != '$' {
		return false
	}
	for _, character := range variable[1:] {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func expandSwayInclude(value string, variables map[string]string) (string, error) {
	if strings.Contains(value, "`") || strings.Contains(value, "$(") || strings.Contains(value, "${") {
		return "", errors.New("command or braced expansion is unsupported")
	}
	var expanded strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '$' {
			expanded.WriteByte(value[index])
			index++
			continue
		}
		end := index + 1
		for end < len(value) && ((value[end] >= 'a' && value[end] <= 'z') ||
			(value[end] >= 'A' && value[end] <= 'Z') || (value[end] >= '0' && value[end] <= '9') || value[end] == '_') {
			end++
		}
		if end == index+1 {
			return "", errors.New("unsupported variable reference")
		}
		variable := value[index:end]
		replacement, ok := variables[variable]
		if !ok {
			return "", fmt.Errorf("unknown variable %s", variable)
		}
		expanded.WriteString(replacement)
		index = end
	}
	result := expanded.String()
	if result == "~" || strings.HasPrefix(result, "~/") {
		home, ok := variables["$HOME"]
		if !ok {
			return "", errors.New("home directory is unavailable")
		}
		if result == "~" {
			result = home
		} else {
			result = filepath.Join(home, strings.TrimPrefix(result, "~/"))
		}
	} else if strings.HasPrefix(result, "~") {
		return "", errors.New("named-user home expansion is unsupported")
	}
	if strings.ContainsRune(result, '\x00') || strings.ContainsAny(result, "\r\n") {
		return "", errors.New("include path contains control characters")
	}
	return result, nil
}

func tokenizeSwayLine(line string) ([]string, bool, error) {
	var tokens []string
	var token strings.Builder
	quote := byte(0)
	escaped := false
	haveToken := false
	for index := 0; index < len(line); index++ {
		character := line[index]
		if escaped {
			token.WriteByte(character)
			haveToken = true
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			haveToken = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				token.WriteByte(character)
			}
			haveToken = true
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
			haveToken = true
		case '#':
			index = len(line)
		case ';':
			return nil, true, nil
		case ' ', '\t', '\r':
			if haveToken {
				tokens = append(tokens, token.String())
				token.Reset()
				haveToken = false
			}
		default:
			token.WriteByte(character)
			haveToken = true
		}
	}
	if escaped {
		return nil, true, nil
	}
	if quote != 0 {
		return nil, false, errors.New("unterminated quoted string")
	}
	if haveToken {
		tokens = append(tokens, token.String())
	}
	return tokens, false, nil
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
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
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
