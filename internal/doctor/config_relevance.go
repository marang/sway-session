package doctor

import (
	"path/filepath"
	"strings"
)

const (
	maxSwayWrapperDepth = 4
	maxSwayWrapperWork  = 256 << 10
)

// This is a relevance recognizer, not a shell interpreter. Only literal command
// positions, assignments and exec/command/env/shell envelopes are recognized.
// Unsupported or exhausted envelopes are potentially relevant. Every expansion,
// payload scan and recursive shell/env transition shares this one budget.
type swayRelevance struct {
	variables  map[string]string
	remaining  int
	unfinished bool // Only the current command's final literal word is incomplete.
}

func execReferencesSwaySession(tokens []string, variables map[string]string, raw ...string) bool {
	recognizer := swayRelevance{variables: variables, remaining: maxSwayWrapperWork}
	if len(raw) != 0 && raw[0] != "" {
		// A block prefix can exceed the raw logical-line bound even when its
		// decoded tokens fit. Never fall back to tokens after losing that syntax.
		if len(raw[0]) > maxSwayConfigLine {
			return true
		}
		if payload := swayExecPayload(raw[0]); payload != "" {
			return recognizer.payload(payload, 0)
		}
	}
	return recognizer.command(skipExecOptions(tokens), 0)
}

// Parse errors may still leave trustworthy literal executable positions. Only
// unfinished quoting/escaping retains words; unsupported dynamic syntax retains
// nothing. A malformed argument cannot turn a known unrelated program into an
// integration declaration, but an incomplete executable or wrapper can.
func execReferencesSwaySessionAfterParseError(tokens []string, variables map[string]string, raw string) bool {
	recognizer := swayRelevance{variables: variables, remaining: maxSwayWrapperWork}
	if len(raw) > maxSwayConfigLine {
		return true
	}
	payload := swayExecPayload(raw)
	if payload == "" {
		recognizer.unfinished = true
		return recognizer.command(skipExecOptions(tokens), 0)
	}
	payload, bounded := recognizer.boundedPayload(payload)
	if !bounded {
		return true
	}
	commands, complete := literalShellCommands(payload)
	if !complete && len(commands) == 0 {
		return true
	}
	commands, balanced := literalShellControlCommands(commands, maxSwayWrapperDepth)
	if !balanced {
		return true
	}
	for index, command := range commands {
		recognizer.unfinished = !complete && index == len(commands)-1
		if recognizer.command(command, 0) {
			return true
		}
	}
	return false
}
func (recognizer *swayRelevance) spend(bytes int) bool {
	if bytes > recognizer.remaining {
		return false
	}
	recognizer.remaining -= bytes
	return true
}
func (recognizer *swayRelevance) expand(token string) (string, bool) {
	if !recognizer.spend(len(token) + 1) {
		return "", false
	}
	expanded, err := expandSwayScalar(token, recognizer.variables, min(maxSwayExpansionBytes, recognizer.remaining-1))
	if err != nil || !recognizer.spend(len(expanded)+1) {
		return "", false
	}
	return expanded, true
}
func (recognizer *swayRelevance) command(tokens []string, depth int) bool {
	if depth > maxSwayWrapperDepth {
		return true
	}
	if len(tokens) == 0 {
		return false
	}
	if recognizer.unfinished && len(tokens) == 1 {
		return true
	}
	executable, ok := recognizer.expand(tokens[0])
	if !ok {
		return true
	}
	if shellAssignment(executable) {
		return recognizer.command(tokens[1:], depth+1)
	}
	switch filepath.Base(executable) {
	case "if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "function", "eval", "source", ".", "!", "time", "{", "}":
		return true // Shell control/evaluation is outside the literal grammar.
	case "sway-session":
		return true
	case "env":
		return recognizer.env(tokens[1:], depth+1)
	case "sh", "bash", "zsh", "fish":
		return recognizer.shell(tokens[1:], depth+1)
	case "exec", "command":
		index := 1
		for index < len(tokens) && strings.HasPrefix(tokens[index], "-") {
			if !recognizer.spend(len(tokens[index]) + 1) {
				return true
			}
			if tokens[index] != "--" && tokens[index] != "-p" {
				return true
			}
			index++
		}
		return recognizer.command(tokens[index:], depth+1)
	}
	if strings.HasPrefix(executable, "-") {
		return true
	}
	// Quoted whole commands or separators attached to an executable token can
	// hide a later executable. Ordinary arguments are deliberately not searched.
	if strings.ContainsAny(executable, " \t\n;|&()<>") {
		return recognizer.payload(executable, depth+1)
	}
	return false
}
func (recognizer *swayRelevance) env(tokens []string, depth int) bool {
	if depth > maxSwayWrapperDepth {
		return true
	}
	for index := 0; index < len(tokens); index++ {
		token, ok := recognizer.expand(tokens[index])
		if !ok {
			return true
		}
		if shellAssignment(token) {
			continue
		}
		switch token {
		case "-i", "--ignore-environment", "-0", "--null":
			continue
		case "--":
			return recognizer.command(tokens[index+1:], depth)
		case "-u", "-C", "-a", "--unset", "--chdir", "--argv0":
			index++
			if index >= len(tokens) {
				return true
			}
			if _, ok := recognizer.expand(tokens[index]); !ok {
				return true
			}
			continue
		case "-S", "--split-string":
			if index+1 >= len(tokens) {
				return true
			}
			return recognizer.split(tokens[index+1], tokens[index+2:], depth+1)
		}
		if strings.HasPrefix(token, "--split-string=") {
			return recognizer.split(strings.TrimPrefix(token, "--split-string="), tokens[index+1:], depth+1)
		}
		if strings.HasPrefix(token, "-S") && len(token) > 2 {
			return recognizer.split(token[2:], tokens[index+1:], depth+1)
		}
		if strings.HasPrefix(token, "-") {
			// Attached values for supported options do not consume another argv.
			if strings.HasPrefix(token, "--unset=") || strings.HasPrefix(token, "--chdir=") || strings.HasPrefix(token, "--argv0=") ||
				strings.HasPrefix(token, "-u") || strings.HasPrefix(token, "-C") || strings.HasPrefix(token, "-a") {
				continue
			}
			return true
		}
		return recognizer.command(tokens[index:], depth)
	}
	return false
}
func (recognizer *swayRelevance) split(payload string, remaining []string, depth int) bool {
	if depth > maxSwayWrapperDepth || recognizer.unfinished {
		return true
	}
	expanded, ok := recognizer.expand(payload)
	if !ok || !recognizer.spend(len(expanded)+1) {
		return true
	}
	lexed := lexSwayLine(expanded)
	if lexed.err != nil || lexed.brace != 0 || lexed.embeddedBrace || len(lexed.tokens) == 0 {
		return true
	}
	command := make([]string, 0, len(lexed.tokens)+len(remaining))
	command = append(command, lexed.tokens...)
	command = append(command, remaining...)
	return recognizer.env(command, depth)
}
func (recognizer *swayRelevance) shell(tokens []string, depth int) bool {
	if depth > maxSwayWrapperDepth {
		return true
	}
	for index, token := range tokens {
		if !recognizer.spend(len(token) + 1) {
			return true
		}
		switch token {
		case "-l", "-i", "-e", "-u", "-x", "-eu", "-eux":
			continue
		case "-c", "-lc", "-ic", "-xc", "-ec", "-uc", "-vc":
			if index+1 >= len(tokens) {
				return true
			}
			if recognizer.unfinished && index+1 == len(tokens)-1 {
				return true
			}
			return recognizer.payload(tokens[index+1], depth)
		default:
			if strings.HasPrefix(token, "-") {
				return true
			}
			// A literal script path is not a shell command payload. Do not read
			// its contents or search its ordinary argv for program names.
			if recognizer.unfinished && index == len(tokens)-1 {
				return true
			}
			script, ok := recognizer.expand(token)
			return !ok || filepath.Base(script) == "sway-session"
		}
	}
	return true
}
func (recognizer *swayRelevance) payload(payload string, depth int) bool {
	if depth > maxSwayWrapperDepth {
		return true
	}
	payload, bounded := recognizer.boundedPayload(payload)
	if !bounded {
		return true
	}
	commands, complete := literalShellCommands(payload)
	if !complete {
		return true
	}
	commands, complete = literalShellControlCommands(commands, maxSwayWrapperDepth-depth)
	if !complete {
		return true
	}
	for _, command := range commands {
		if recognizer.command(command, depth) {
			return true
		}
	}
	return false
}

// Sway substitutes argument variables before shell parsing, including inside
// quotes. Their values can introduce command separators or quote boundaries.
// Substitute known variables across the bounded payload first. Unset references
// stay literal for executable-position checks; nested or dynamic values remain
// unknown. This also preserves unrelated programs' quoted shell-variable argv.
func (recognizer *swayRelevance) boundedPayload(payload string) (string, bool) {
	if len(payload) > maxSwayConfigLine || !recognizer.spend(len(payload)+1) {
		return "", false
	}
	if !strings.ContainsRune(payload, '$') {
		return payload, true
	}
	limit := min(maxSwayExpansionBytes, recognizer.remaining-1)
	var expanded strings.Builder
	write := func(value string) bool {
		if len(value) > limit-expanded.Len() {
			return false
		}
		expanded.WriteString(value)
		return true
	}
	for index := 0; index < len(payload); {
		if payload[index] != '$' {
			if !write(payload[index : index+1]) {
				return "", false
			}
			index++
			continue
		}
		end := index + 1
		for end < len(payload) && ((payload[end] >= 'a' && payload[end] <= 'z') || (payload[end] >= 'A' && payload[end] <= 'Z') || (payload[end] >= '0' && payload[end] <= '9') || payload[end] == '_') {
			end++
		}
		name := payload[index:end]
		replacement, known := recognizer.variables[name]
		if !known {
			// A defined shorter prefix could have Sway substitution semantics
			// outside this exact-name grammar. Do not silently ignore it.
			for prefix := len(name) - 1; prefix > 1; prefix-- {
				if !recognizer.spend(prefix) {
					return "", false
				}
				if _, found := recognizer.variables[name[:prefix]]; found {
					return "", false
				}
			}
			replacement = name
		} else if strings.ContainsAny(replacement, "$`\x00\r\n") {
			return "", false
		}
		if !write(replacement) {
			return "", false
		}
		index = end
	}
	return expanded.String(), recognizer.spend(expanded.Len() + 1)
}

// Only tokenize literal words and command separators. Quotes protect argument
// separators. Executable-position scalar variables are recognized separately;
// substitutions and unsupported programming constructs remain unresolved.
// Literal redirection targets are arguments, never executable positions.
// Nothing is evaluated, launched or read from a script file.
func literalShellCommands(payload string) ([][]string, bool) {
	var commands [][]string
	var words []string
	var word strings.Builder
	var quote byte
	escaped, active := false, false
	finishWord := func() {
		if active {
			words = append(words, word.String())
			word.Reset()
			active = false
		}
	}
	finishCommand := func() {
		finishWord()
		if len(words) != 0 {
			commands = append(commands, words)
			words = nil
		}
	}
	for index := 0; index < len(payload); index++ {
		character := payload[index]
		if escaped {
			word.WriteByte(character)
			active = true
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped, active = true, true
			continue
		}
		if (character == '`' && quote != '\'') || (character == '$' && quote != '\'' && index+1 < len(payload) && payload[index+1] == '(') || character == 0 {
			return nil, false
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				word.WriteByte(character)
			}
			active = true
			continue
		}
		switch character {
		case '\'', '"':
			quote, active = character, true
		case ' ', '\t', '\r':
			finishWord()
		case ';', '|', '(', ')', '\n':
			finishCommand()
		case '&':
			if index+1 < len(payload) && payload[index+1] == '>' {
				finishWord()
				next, complete := literalShellRedirectEnd(payload, index)
				if !complete {
					return nil, false
				}
				index = next - 1
			} else {
				finishCommand()
			}
		case '<', '>':
			// A descriptor immediately adjacent to the operator is syntax.
			// Other adjacent text remains an ordinary command/argument word.
			if active && decimalShellFD(word.String()) {
				word.Reset()
				active = false
			} else {
				finishWord()
			}
			next, complete := literalShellRedirectEnd(payload, index)
			if !complete {
				return nil, false
			}
			index = next - 1
		case '#':
			if !active {
				index = len(payload)
			} else {
				word.WriteByte(character)
			}
		default:
			word.WriteByte(character)
			active = true
		}
	}
	if escaped || quote != 0 {
		finishCommand()
		return commands, false
	}
	finishCommand()
	return commands, true
}

// Recognize a file target or literal descriptor duplication/closure. Heredocs,
// process substitution, missing targets and command substitutions are opaque.
// The scan only consumes input, so its work and output stay within payload bounds.
func literalShellRedirectEnd(payload string, start int) (int, bool) {
	index := start
	if payload[index] == '&' {
		index++ // The caller established the following '>'.
	}
	operator := payload[index]
	index++
	duplicate := false
	if index < len(payload) {
		switch payload[index] {
		case '&':
			duplicate = true
			index++
		case '>':
			index++ // >> or <>.
		case '|':
			if operator != '>' {
				return 0, false
			}
			index++
		case '<':
			return 0, false
		}
	}
	for index < len(payload) && strings.ContainsRune(" \t\r", rune(payload[index])) {
		index++
	}
	var target strings.Builder
	var quote byte
	escaped, active := false, false
	for ; index < len(payload); index++ {
		character := payload[index]
		if escaped {
			target.WriteByte(character)
			active, escaped = true, false
			continue
		}
		if character == '\\' && quote != '\'' {
			active, escaped = true, true
			continue
		}
		if character == 0 || (quote != '\'' && (character == '`' || (character == '$' && index+1 < len(payload) && payload[index+1] == '('))) {
			return 0, false
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				target.WriteByte(character)
			}
			active = true
			continue
		}
		if strings.ContainsRune(" \t\r\n;|&()<>", rune(character)) {
			break
		}
		switch character {
		case '\'', '"':
			quote, active = character, true
		case '#':
			if !active {
				return 0, false
			}
			target.WriteByte(character)
		default:
			target.WriteByte(character)
			active = true
		}
	}
	if !active || escaped || quote != 0 || target.Len() == 0 {
		return 0, false
	}
	if duplicate && target.String() != "-" && !decimalShellFD(target.String()) {
		return 0, false
	}
	return index, true
}

func decimalShellFD(word string) bool {
	if word == "" {
		return false
	}
	for index := 0; index < len(word); index++ {
		if word[index] < '0' || word[index] > '9' {
			return false
		}
	}
	return true
}

// Only locate command positions in balanced if/then/elif/else/fi envelopes.
// All branches and tests are returned, including literal false branches; no
// condition is evaluated. Nesting consumes the remaining wrapper depth budget.
func literalShellControlCommands(commands [][]string, limit int) ([][]string, bool) {
	type frame struct {
		branch  byte // 'i' condition, 't' then branch, 'e' else branch.
		command bool
	}
	var frames []frame
	var positions [][]string
	for _, words := range commands {
		for len(words) != 0 {
			switch words[0] {
			case "if":
				if len(frames) >= limit {
					return nil, false
				}
				if len(frames) != 0 {
					frames[len(frames)-1].command = true
				}
				frames = append(frames, frame{branch: 'i'})
			case "then", "elif", "else", "fi":
				if len(frames) == 0 {
					return nil, false
				}
				top := &frames[len(frames)-1]
				if !top.command {
					return nil, false
				}
				switch words[0] {
				case "then":
					if top.branch != 'i' {
						return nil, false
					}
					top.branch, top.command = 't', false
				case "elif", "else":
					if top.branch != 't' {
						return nil, false
					}
					if words[0] == "elif" {
						top.branch = 'i'
					} else {
						top.branch = 'e'
					}
					top.command = false
				case "fi":
					if top.branch == 'i' || len(words) != 1 {
						return nil, false
					}
					frames = frames[:len(frames)-1]
				}
			default:
				positions = append(positions, words)
				if len(frames) != 0 {
					frames[len(frames)-1].command = true
				}
				words = nil
				continue
			}
			words = words[1:]
		}
	}
	return positions, len(frames) == 0
}

func shellAssignment(token string) bool {
	name, _, found := strings.Cut(token, "=")
	if !found || name == "" {
		return false
	}
	for index, character := range name {
		if !(character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || (index != 0 && character >= '0' && character <= '9')) {
			return false
		}
	}
	return true
}
