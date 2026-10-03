package doctor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	maxSwayExpansionBytes  = 64 << 10
	maxSwayUncertaintyKeys = 4096
)

// The classifier consumes ordered logical lines and returns facts. Its only
// mutable state is the injected variable table and each source's block stack;
// it never reads files, environment, IPC, or repair plans. Include facts must be
// traversed before the next parent line, so assignments have Sway source order.
type swayConfigClassifier struct{ variables map[string]string }
type swayFileClassifier struct {
	variables map[string]string
	path      string
	blocks    []swayBlock
	exhausted bool
	facts     swayConfigFacts
}
type swaySetDirective struct{ name, value string }
type swayConfigLimitation struct {
	kinds  []integrationKind
	where  configLocation
	reason string
}
type swayConfigFacts struct {
	set         *swaySetDirective
	includes    []string
	occurrences []integrationOccurrence
	limitations []swayConfigLimitation
}
type swayBlockKind uint8

const (
	swayBlockOpaque swayBlockKind = iota
	swayBlockMode
	swayBlockBinding
	swayBlockForWindow
	swayBlockStartup
)

type swayModeScope uint8

const (
	swayModeOther swayModeScope = iota
	swayModeDefault
	swayModeUnknown
)

type swayBlock struct {
	kind   swayBlockKind
	mode   swayModeScope
	prefix []string
}

func newSwayConfigClassifier(seeds map[string]string) *swayConfigClassifier {
	variables := make(map[string]string, len(seeds))
	for name, value := range seeds {
		variables[name] = value
	}
	return &swayConfigClassifier{variables: variables}
}
func (classifier *swayConfigClassifier) source(path string) *swayFileClassifier {
	return &swayFileClassifier{path: path, variables: classifier.variables}
}

// Grammar: one logical statement or terminal standalone brace per line;
// literal set/include; direct exec and default bindsym; literal mode blocks;
// output/input/bar opaque scopes; bindsym/exec prefix blocks; conditional
// for_window bodies. Dynamic commands, keycodes/unbinding, unresolved modes,
// and incomplete structure yield limitations rather than effective settings.
func (classifier *swayFileClassifier) line(logical swayLogicalLine) swayConfigFacts {
	classifier.facts = swayConfigFacts{}
	if classifier.exhausted {
		return classifier.facts
	}
	location := configLocation{path: classifier.path, line: logical.start}
	lexed := lexSwayLine(logical.text)
	// status_command consumes its remaining text as a shell payload. Braces
	// inside that payload do not introduce Sway scopes or hide declarations.
	if len(classifier.blocks) != 0 && classifier.blocks[len(classifier.blocks)-1].kind == swayBlockOpaque &&
		len(lexed.tokens) != 0 && strings.EqualFold(lexed.tokens[0], "status_command") && lexed.brace == 0 {
		return classifier.facts
	}
	if lexed.embeddedBrace {
		classifier.markUncertainAll(location, "inline structural syntax requires manual review")
		classifier.exhausted = true
		return classifier.facts
	}
	if lexed.err != nil {
		classifier.markParseError(lexed.tokens, location, lexed.err, logical.text)
		return classifier.facts
	}
	tokens := lexed.tokens
	if len(tokens) == 0 {
		return classifier.facts
	}
	if lexed.brace == '}' {
		if len(classifier.blocks) == 0 {
			classifier.markUncertainAll(location, "unmatched closing block")
		} else {
			block := classifier.blocks[len(classifier.blocks)-1]
			if len(tokens) > 1 && block.kind != swayBlockOpaque {
				classifier.markUncertainAll(location, "a command combined with a block closing brace requires manual review")
			}
			classifier.blocks = classifier.blocks[:len(classifier.blocks)-1]
		}
		return classifier.facts
	}
	if lexed.brace == '{' {
		if len(classifier.blocks) >= maxSwayBlockDepth {
			classifier.markUncertainAll(location, fmt.Sprintf("block nesting exceeds the supported depth of %d", maxSwayBlockDepth))
			classifier.exhausted = true
		} else {
			classifier.blocks = append(classifier.blocks, classifier.classifyBlock(tokens[:len(tokens)-1], classifier.blocks, location))
		}
		return classifier.facts
	}
	if len(classifier.blocks) != 0 {
		block := classifier.blocks[len(classifier.blocks)-1]
		switch block.kind {
		case swayBlockBinding, swayBlockStartup:
			combined, err := combineSwayPrefix(block.prefix, tokens)
			if err != nil {
				classifier.markUncertainAll(location, err.Error())
			} else {
				classifier.recordIntegration(combined, location, strings.Join(block.prefix, " ")+" "+logical.text)
			}
		case swayBlockForWindow:
			combined, err := combineSwayPrefix(block.prefix, tokens)
			if err != nil {
				classifier.markStartupUncertain(location, err.Error())
			} else {
				classifier.recordConditional(combined, location, strings.Join(block.prefix, " ")+" "+logical.text)
			}
		case swayBlockMode:
			if strings.ContainsRune(tokens[0], '$') {
				classifier.markUncertainAll(location, "a variable-derived command in a mode block requires manual review")
			} else if strings.EqualFold(tokens[0], "set") {
				if len(tokens) < 3 || !validSwayVariable(tokens[1]) {
					classifier.markUncertainAll(location, "an unsupported variable assignment in a mode block requires manual review")
				} else {
					classifier.recordSet(tokens, location)
				}
			} else if block.mode == swayModeDefault {
				classifier.recordDefaultModeIntegration(tokens, location)
			} else if block.mode == swayModeUnknown {
				classifier.markPotentialBindingUncertain(tokens, location)
			}
		}
		return classifier.facts
	}
	if strings.ContainsRune(tokens[0], '$') {
		classifier.markUncertainAll(location, "variable-derived command requires manual review")
		return classifier.facts
	}
	tokens[0] = strings.ToLower(tokens[0])
	classifier.recordIntegration(tokens, location, logical.text)
	switch tokens[0] {
	case "set":
		classifier.recordSet(tokens, location)
	case "include":
		classifier.recordInclude(tokens[1:], location)
	case "for_window":
		// A literal criterion ends before its command. Unresolved criteria are
		// outside this grammar and cannot authorize repair.
		end := 1
		for end < len(tokens) && !strings.HasSuffix(tokens[end], "]") {
			end++
		}
		if end == len(tokens) {
			classifier.markStartupUncertain(location, "unresolved for_window criteria")
		} else {
			if end+1 < len(tokens) {
				classifier.recordConditional(tokens[end+1:], location, logical.text[lexed.starts[end+1]:])
			}
		}
	}
	return classifier.facts
}
func combineSwayPrefix(prefix, tokens []string) ([]string, error) {
	size := 0
	for _, group := range [][]string{prefix, tokens} {
		for _, token := range group {
			if len(token)+1 > maxSwayConfigLine-size {
				return nil, errors.New("expanded block prefix exceeds the supported length")
			}
			size += len(token) + 1
		}
	}
	combined := make([]string, 0, len(prefix)+len(tokens))
	combined = append(combined, prefix...)
	return append(combined, tokens...), nil
}
func (classifier *swayFileClassifier) recordConditional(tokens []string, location configLocation, raw string) {
	if len(tokens) == 0 {
		return
	}
	if strings.ContainsRune(tokens[0], '$') {
		expanded, err := expandSwayInclude(strings.Join(tokens, " "), classifier.variables)
		if err != nil {
			classifier.markStartupUncertain(location, "an unresolved variable-derived command in a for_window block requires manual startup review")
			return
		}
		lexed := lexSwayLine(expanded)
		if lexed.err != nil || lexed.embeddedBrace || lexed.brace != 0 {
			classifier.markStartupUncertain(location, "a variable-derived command in a for_window block requires manual startup review")
			return
		}
		tokens = lexed.tokens
		raw = expanded
	}
	if len(tokens) != 0 && (strings.EqualFold(tokens[0], "exec") || strings.EqualFold(tokens[0], "exec_always")) && execReferencesSwaySession(tokens[1:], classifier.variables, raw) {
		classifier.markStartupUncertain(location, "a conditional sway-session startup in a for_window block requires manual review")
	}
}
func (classifier *swayFileClassifier) markParseError(tokens []string, location configLocation, parseErr error, raw string) {
	reason := "cannot safely parse declaration: " + parseErr.Error()
	var mode *swayBlock
	if len(classifier.blocks) != 0 {
		block := classifier.blocks[len(classifier.blocks)-1]
		switch block.kind {
		case swayBlockOpaque:
			return
		case swayBlockBinding, swayBlockStartup, swayBlockForWindow:
			combined, err := combineSwayPrefix(block.prefix, tokens)
			if err != nil {
				classifier.markUncertainAll(location, err.Error())
				return
			}
			tokens = combined
			raw = strings.Join(block.prefix, " ") + " " + raw
			if block.kind == swayBlockForWindow {
				if len(tokens) == 0 || strings.ContainsRune(tokens[0], '$') || ((strings.EqualFold(tokens[0], "exec") || strings.EqualFold(tokens[0], "exec_always")) && execReferencesSwaySessionAfterParseError(tokens[1:], classifier.variables, raw)) {
					classifier.markStartupUncertain(location, reason)
				}
				return
			}
		case swayBlockMode:
			mode = &block
		}
	}
	if len(tokens) == 0 || strings.ContainsRune(tokens[0], '$') {
		classifier.markUncertainAll(location, reason)
		return
	}
	command := strings.ToLower(tokens[0])
	if mode != nil && mode.mode == swayModeOther && command != "set" {
		return
	}
	switch command {
	case "bindsym":
		copyTokens := append([]string(nil), tokens...)
		copyTokens[0] = command
		_, relevant, _, err := classifyBinding(copyTokens, classifier.variables)
		if relevant || err != nil || len(tokens) < 3 {
			classifier.markShortcutUncertain(location, reason)
		}
	case "bindcode", "unbindcode", "unbindsym", "mode":
		classifier.markShortcutUncertain(location, reason)
	case "exec", "exec_always":
		if mode == nil && execReferencesSwaySessionAfterParseError(tokens[1:], classifier.variables, raw) {
			classifier.markStartupUncertain(location, reason)
		}
	case "include", "set":
		classifier.markUncertainAll(location, reason)
	case "for_window":
		classifier.markStartupUncertain(location, reason)
	}
}
func (classifier *swayFileClassifier) finish() swayConfigFacts {
	classifier.facts = swayConfigFacts{}
	if len(classifier.blocks) != 0 {
		classifier.markUncertainAll(configLocation{path: classifier.path}, "unterminated block")
	}
	return classifier.facts
}
func (classifier *swayFileClassifier) recordInclude(patterns []string, location configLocation) {
	if len(patterns) == 0 {
		classifier.markUncertainAll(location, "include has no path")
		return
	}
	total := 0
	for _, pattern := range patterns {
		expanded, err := expandSwayInclude(pattern, classifier.variables)
		if err != nil {
			classifier.markUncertainAll(location, "cannot safely expand include: "+err.Error())
			continue
		}
		if len(expanded)+1 > maxSwayExpansionBytes-total {
			classifier.markUncertainAll(location, "expanded include directives exceed the supported length")
			break
		}
		total += len(expanded) + 1
		classifier.facts.includes = append(classifier.facts.includes, expanded)
	}
}
func (classifier *swayFileClassifier) markUncertain(kinds []integrationKind, location configLocation, reason string) {
	classifier.facts.limitations = append(classifier.facts.limitations, swayConfigLimitation{kinds: kinds, where: location, reason: reason})
}
func (classifier *swayFileClassifier) markUncertainAll(location configLocation, reason string) {
	classifier.markUncertain(integrationOrder, location, reason)
}
func (classifier *swayFileClassifier) markStartupUncertain(location configLocation, reason string) {
	classifier.markUncertain([]integrationKind{integrationDaemon, integrationRestore}, location, reason)
}
func (classifier *swayFileClassifier) markShortcutUncertain(location configLocation, reason string) {
	classifier.markUncertain([]integrationKind{integrationPersistent, integrationEphemeral}, location, reason)
}
func (classifier *swayFileClassifier) recordOccurrence(kind integrationKind, correct bool, location configLocation) {
	classifier.facts.occurrences = append(classifier.facts.occurrences, integrationOccurrence{kind: kind, correct: correct, where: location})
}
func (classifier *swayFileClassifier) classifyBlock(header []string, blocks []swayBlock, location configLocation) swayBlock {
	if len(blocks) != 0 {
		parent := blocks[len(blocks)-1]
		switch parent.kind {
		case swayBlockOpaque:
			return swayBlock{kind: swayBlockOpaque}
		case swayBlockBinding, swayBlockStartup:
			combined, err := combineSwayPrefix(parent.prefix, header)
			if err != nil {
				classifier.markUncertainAll(location, err.Error())
				return swayBlock{kind: swayBlockOpaque}
			}
			header = combined
		case swayBlockForWindow:
			combined, err := combineSwayPrefix(parent.prefix, header)
			if err != nil {
				classifier.markStartupUncertain(location, err.Error())
				return swayBlock{kind: swayBlockOpaque}
			}
			return swayBlock{kind: swayBlockForWindow, prefix: combined}
		case swayBlockMode:
			classifier.markShortcutUncertain(location, "nested syntax in a mode block requires manual shortcut review")
			return swayBlock{kind: swayBlockOpaque}
		}
	}
	if len(header) == 0 {
		classifier.markUncertainAll(location, "a block without a command requires manual review")
		return swayBlock{kind: swayBlockOpaque}
	}
	command := strings.ToLower(header[0])
	if strings.ContainsRune(command, '$') {
		classifier.markUncertainAll(location, "a variable-derived block command requires manual review")
		return swayBlock{kind: swayBlockOpaque}
	}
	switch command {
	case "mode":
		return swayBlock{kind: swayBlockMode, mode: classifier.classifyModeBlock(header, location)}
	case "output", "input", "bar":
		return swayBlock{kind: swayBlockOpaque}
	case "for_window":
		return swayBlock{kind: swayBlockForWindow}
	case "bindsym":
		return swayBlock{kind: swayBlockBinding, prefix: append([]string(nil), header...)}
	case "bindcode", "unbindsym", "unbindcode":
		classifier.markShortcutUncertain(location, "a compound shortcut block requires manual review")
	case "exec", "exec_always":
		return swayBlock{kind: swayBlockStartup, prefix: append([]string(nil), header...)}
	default:
		classifier.markUncertainAll(location, "an unsupported block command requires manual review")
	}
	return swayBlock{kind: swayBlockOpaque}
}

func (classifier *swayFileClassifier) classifyModeBlock(header []string, location configLocation) swayModeScope {
	index := 1
	if index < len(header) && strings.EqualFold(header[index], "--pango_markup") {
		index++
	}
	if index != len(header)-1 {
		classifier.markShortcutUncertain(location, "the binding mode name could not be resolved")
		return swayModeUnknown
	}
	if strings.ContainsRune(header[index], '$') {
		classifier.markShortcutUncertain(location, "a variable-derived binding mode name requires manual shortcut review")
		return swayModeUnknown
	}
	name, err := expandSwayInclude(header[index], classifier.variables)
	if err != nil {
		classifier.markShortcutUncertain(location, "the binding mode name could not be resolved: "+err.Error())
		return swayModeUnknown
	}
	if name == "default" {
		return swayModeDefault
	}
	return swayModeOther
}

func (classifier *swayFileClassifier) recordSet(tokens []string, location configLocation) {
	if len(tokens) < 3 || !strings.HasPrefix(tokens[1], "$") || !validSwayVariable(tokens[1]) {
		classifier.markUncertainAll(location, "an unsupported variable assignment requires manual review")
		return
	}
	value := strings.Join(tokens[2:], " ")
	if tokens[1] == "$mod" {
		if previous := classifier.variables["$mod"]; previous != "" && previous != value {
			classifier.markShortcutUncertain(location, "changing $mod definitions require manual shortcut review")
		}
	}
	classifier.variables[tokens[1]] = value
	classifier.facts.set = &swaySetDirective{name: tokens[1], value: value}
}

func (classifier *swayFileClassifier) markPotentialBindingUncertain(tokens []string, location configLocation) {
	if len(tokens) == 0 {
		return
	}
	command := strings.ToLower(tokens[0])
	if strings.ContainsRune(command, '$') {
		classifier.markShortcutUncertain(location, "a variable-derived command in an unresolved mode requires manual shortcut review")
		return
	}
	switch command {
	case "bindcode", "unbindcode", "unbindsym":
		classifier.markShortcutUncertain(location, "a shortcut in an unresolved binding mode requires manual review")
	case "bindsym":
		copyTokens := append([]string(nil), tokens...)
		copyTokens[0] = command
		_, relevant, _, err := classifyBinding(copyTokens, classifier.variables)
		if err != nil || relevant {
			classifier.markShortcutUncertain(location, "a shortcut in an unresolved binding mode requires manual review")
		}
	}
}

func (classifier *swayFileClassifier) recordDefaultModeIntegration(tokens []string, location configLocation) {
	if len(tokens) == 0 {
		return
	}
	command := strings.ToLower(tokens[0])
	if strings.ContainsRune(command, '$') {
		classifier.markShortcutUncertain(location, "a variable-derived default-mode command requires manual shortcut review")
		return
	}
	switch command {
	case "bindsym", "bindcode", "unbindsym", "unbindcode":
		classifier.recordIntegration(tokens, location, "")
	}
}

func (classifier *swayFileClassifier) recordIntegration(tokens []string, location configLocation, raw string) {
	if len(tokens) == 0 {
		return
	}
	tokens = append([]string(nil), tokens...)
	tokens[0] = strings.ToLower(tokens[0])
	if tokens[0] == "mode" {
		classifier.markShortcutUncertain(location, "binding mode selection requires manual shortcut review")
		return
	}
	if tokens[0] == "bindcode" || tokens[0] == "unbindcode" || tokens[0] == "unbindsym" {
		classifier.markShortcutUncertain(location, "keycode or unbinding syntax requires manual shortcut review")
		return
	}
	references := (tokens[0] == "exec" || tokens[0] == "exec_always") && execReferencesSwaySession(tokens[1:], classifier.variables, raw)
	if references {
		for _, token := range tokens[1:] {
			if strings.ContainsAny(token, "$`") {
				classifier.markStartupUncertain(location,
					fmt.Sprintf("variable or shell-derived sway-session startup at %s:%d requires manual review", location.path, location.line))
				return
			}
		}
	}
	if kind, relevant, correct := classifyStartup(tokens); relevant {
		classifier.recordOccurrence(kind, correct, location)
	} else if references {
		classifier.markStartupUncertain(location,
			fmt.Sprintf("indirect sway-session startup at %s:%d requires manual review", location.path, location.line))
		return
	}
	kind, relevant, correct, err := classifyBinding(tokens, classifier.variables)
	if err != nil {
		classifier.markShortcutUncertain(location,
			fmt.Sprintf("cannot safely resolve shortcut at %s:%d: %v", location.path, location.line, err))
		return
	}
	if relevant {
		classifier.recordOccurrence(kind, correct, location)
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

func classifyBinding(tokens []string, variables map[string]string) (integrationKind, bool, bool, error) {
	if len(tokens) < 3 || tokens[0] != "bindsym" {
		return 0, false, false, nil
	}
	index := 1
	ordinaryOptions := true
	for index < len(tokens) && strings.HasPrefix(tokens[index], "--") {
		switch tokens[index] {
		case "--no-warn", "--inhibited":
		case "--release", "--locked", "--no-repeat", "--whole-window", "--border", "--exclude-titlebar", "--to-code":
			ordinaryOptions = false
		default:
			if !strings.HasPrefix(tokens[index], "--input-device=") {
				return 0, false, false, errors.New("unknown binding option requires manual review")
			}
			ordinaryOptions = false
		}
		index++
	}
	if index >= len(tokens) {
		return 0, false, false, nil
	}
	key, err := expandSwayInclude(tokens[index], variables)
	if err != nil {
		return 0, false, false, err
	}
	parts := strings.Split(key, "+")
	keyMask := uint16(0)
	returnKey := false
	unknownPart := false
	for _, part := range parts {
		if strings.EqualFold(part, "Return") {
			returnKey = true
			continue
		}
		mask, ok := modifierMask(part)
		if !ok {
			unknownPart = true
			continue
		}
		keyMask |= mask
	}
	if !returnKey {
		return 0, false, false, nil
	}
	mod, ok := modifierMask(variables["$mod"])
	if !ok || mod&1 != 0 {
		return 0, false, false, errors.New("a known $mod definition without Shift is required to verify the default shortcuts")
	}
	if unknownPart {
		return 0, false, false, errors.New("unsupported Return shortcut requires manual review")
	}
	var kind integrationKind
	switch keyMask {
	case mod:
		kind = integrationPersistent
	case mod | 1:
		kind = integrationEphemeral
	default:
		return 0, false, false, nil
	}
	command := tokens[index+1:]
	if !ordinaryOptions {
		return kind, true, false, nil
	}
	if len(command) == 0 || !strings.EqualFold(command[0], "exec") {
		return kind, true, false, nil
	}
	command = skipExecOptions(command[1:])
	if len(command) != 3 || filepath.Base(command[0]) != "sway-session" || command[1] != "terminal" {
		return kind, true, false, nil
	}
	if kind == integrationPersistent {
		return kind, true, command[2] == "--new", nil
	}
	return kind, true, command[2] == "--ephemeral", nil
}

func modifierMask(value string) (uint16, bool) {
	var mask uint16
	for _, part := range strings.Split(value, "+") {
		switch strings.ToLower(part) {
		case "shift":
			mask |= 1
		case "lock":
			mask |= 2
		case "control", "ctrl":
			mask |= 4
		case "mod1":
			mask |= 8
		case "mod2":
			mask |= 16
		case "mod3":
			mask |= 32
		case "mod4":
			mask |= 64
		case "mod5":
			mask |= 128
		default:
			return 0, false
		}
	}
	return mask, mask != 0
}

func skipExecOptions(tokens []string) []string {
	if len(tokens) > 0 && tokens[0] == "--no-startup-id" {
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
	return expandSwayScalar(value, variables, maxSwayExpansionBytes)
}

func expandSwayScalar(value string, variables map[string]string, limit int) (string, error) {
	if limit < 0 || len(value) > limit {
		return "", errors.New("expansion exceeds the supported length")
	}
	if strings.Contains(value, "`") || strings.Contains(value, "$(") || strings.Contains(value, "${") {
		return "", errors.New("command or braced expansion is unsupported")
	}
	var expanded strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '$' {
			if expanded.Len() == limit {
				return "", errors.New("expansion exceeds the supported length")
			}
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
		if len(replacement) > limit-expanded.Len() {
			return "", errors.New("expansion exceeds the supported length")
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
			if len(home) > limit {
				return "", errors.New("home expansion exceeds the supported length")
			}
			result = home
		} else {
			if len(home)+len(result)-1 > limit {
				return "", errors.New("home expansion exceeds the supported length")
			}
			result = filepath.Join(home, strings.TrimPrefix(result, "~/"))
		}
	} else if strings.HasPrefix(result, "~") {
		return "", errors.New("named-user home expansion is unsupported")
	}
	if strings.ContainsAny(result, "$`") {
		return "", errors.New("nested variable or command expansion requires manual review")
	}
	if strings.ContainsRune(result, '\x00') || strings.ContainsAny(result, "\r\n") {
		return "", errors.New("include path contains control characters")
	}
	return result, nil
}

type swayConfigEvidence struct {
	occurrences          map[integrationKind][]integrationOccurrence
	occurrenceCounts     map[integrationKind]int
	matchingCounts       map[integrationKind]int
	uncertain            map[integrationKind][]configUncertainty
	uncertaintyCounts    map[integrationKind]int
	uncertaintySeen      map[swayUncertaintyKey]struct{}
	uncertaintySaturated map[integrationKind]bool
	unsupported          error
}

type swayUncertaintyKey struct {
	kind   integrationKind
	where  configLocation
	reason string
}

func newSwayConfigEvidence() swayConfigEvidence {
	return swayConfigEvidence{
		occurrences:      make(map[integrationKind][]integrationOccurrence),
		occurrenceCounts: make(map[integrationKind]int), matchingCounts: make(map[integrationKind]int),
		uncertain: make(map[integrationKind][]configUncertainty), uncertaintyCounts: make(map[integrationKind]int),
		uncertaintySeen: make(map[swayUncertaintyKey]struct{}), uncertaintySaturated: make(map[integrationKind]bool),
	}
}
func (evidence *swayConfigEvidence) record(facts swayConfigFacts) {
	for _, occurrence := range facts.occurrences {
		evidence.recordOccurrence(occurrence.kind, occurrence.correct, occurrence.where)
	}
	for _, limitation := range facts.limitations {
		evidence.markUncertain(limitation.kinds, limitation.where, limitation.reason)
	}
}
func (evidence *swayConfigEvidence) markUncertain(kinds []integrationKind, location configLocation, reason string) {
	if evidence.unsupported == nil {
		evidence.unsupported = errors.New(reason)
	}
	for _, kind := range kinds {
		key := swayUncertaintyKey{kind: kind, where: location, reason: reason}
		if _, seen := evidence.uncertaintySeen[key]; seen {
			continue
		}
		retainedReason := reason
		if len(evidence.uncertaintySeen) == maxSwayUncertaintyKeys {
			if evidence.uncertaintySaturated[kind] {
				continue
			}
			evidence.uncertaintySaturated[kind] = true
			retainedReason = "diagnostic deduplication limit reached; additional limitations omitted"
		} else {
			evidence.uncertaintySeen[key] = struct{}{}
		}
		evidence.uncertaintyCounts[kind]++
		if len(evidence.uncertain[kind]) < maxSwayEvidenceItems {
			evidence.uncertain[kind] = append(evidence.uncertain[kind], configUncertainty{
				where: location, reason: retainedReason,
			})
		}
	}
}

func (evidence *swayConfigEvidence) recordOccurrence(kind integrationKind, correct bool, location configLocation) {
	evidence.occurrenceCounts[kind]++
	if correct {
		evidence.matchingCounts[kind]++
	}
	if len(evidence.occurrences[kind]) < maxSwayEvidenceItems {
		evidence.occurrences[kind] = append(evidence.occurrences[kind], integrationOccurrence{
			kind: kind, correct: correct, where: location,
		})
	}
}

func (evidence *swayConfigEvidence) markUncertainAll(location configLocation, reason string) {
	evidence.markUncertain(integrationOrder, location, reason)
}
