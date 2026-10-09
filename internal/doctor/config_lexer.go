package doctor

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
)

// Sway separates source tokens with ASCII whitespace. Unicode whitespace can
// belong to a literal filename and must not be stripped or treated as a delimiter.
const swayConfigWhitespace = " \f\n\r\t\v"

// literalDirectInclude finds only an ordinary, top-level literal include in
// the selected main file. It does not expand variables, follow includes, parse
// shell commands, or infer whether Sway loaded this source.
func literalDirectInclude(content []byte, main, snippet string) int {
	line, _ := inspectDirectInclude(content, main, snippet)
	return line
}

// The append guard establishes only a standalone top-level EOF position. It
// does not validate Sway commands or interpret their arguments.
func inspectDirectInclude(content []byte, main, snippet string) (int, error) {
	// Sway's C-string reader truncates at NUL. Such source cannot establish
	// trustworthy include or append framing for this bounded recognizer.
	if bytes.IndexByte(content, 0) != -1 {
		return 0, errors.New("cannot append a direct include to configuration containing NUL bytes; remove them manually first")
	}
	depth := 0
	var logical strings.Builder
	start, found := 1, 0
	continued := false
	var appendErr error
	lines := strings.Split(string(content), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // a final newline is not another physical line
	}
	for index, physical := range lines {
		// Sway folds continuations before trimming: only a '#' at the
		// beginning of the accumulated raw line suppresses continuation.
		comment := strings.HasPrefix(physical, "#")
		if logical.Len() != 0 {
			comment = strings.HasPrefix(logical.String(), "#")
		}
		continuation := !comment && strings.HasSuffix(physical, "\\")
		part := physical
		if continuation {
			part = strings.TrimSuffix(part, "\\")
		}
		if logical.Len()+len(part) > maxSwayConfigLine {
			// Skipping an uninspected line could lose a block opener. Keep
			// earlier evidence, but do not discover includes after this point.
			return found, errors.New("cannot append a direct include after a line beyond the supported inspection bound")
		}
		logical.WriteString(part)
		if continuation {
			continued = true
			continue
		}
		line := strings.Trim(logical.String(), swayConfigWhitespace)
		if line != "" && !strings.HasPrefix(line, "#") {
			commandEnd := strings.IndexAny(line, swayConfigWhitespace)
			if commandEnd == -1 {
				commandEnd = len(line)
			}
			command := strings.ToLower(line[:commandEnd])
			if depth == 0 && !continued && command == "include" && !nextLineOpensBlock(lines[index+1:]) {
				if path, ok := literalIncludePath(line[commandEnd:]); ok {
					if !filepath.IsAbs(path) {
						path = filepath.Join(filepath.Dir(main), path)
					}
					if path == snippet && found == 0 {
						found = start
					}
				}
			}
			// An unquoted trailing brace opens a Sway source prefix block,
			// including for exec. Quoted shell braces remain opaque payload.
			if line == "}" {
				if depth > 0 {
					depth--
				} else {
					appendErr = errors.New("cannot establish a top-level append position after an unmatched block terminator")
				}
			} else if hasTrailingBlockBrace(line) {
				depth++
			}
		}
		logical.Reset()
		continued = false
		start = index + 2
		if depth > maxSwayBlockDepth {
			// The source is beyond this bounded recognizer. Any earlier
			// evidence remains source evidence, without a load-order claim.
			return found, errors.New("cannot append a direct include beyond the supported block depth")
		}
	}
	if continued {
		appendErr = errors.New("cannot append a direct include after an unfinished line continuation; finish it manually first")
	}
	if depth != 0 {
		appendErr = errors.New("cannot append a direct include inside an unfinished configuration block; close it manually first")
	}
	return found, appendErr
}

// Sway's brace lookahead skips empty physical lines, but stops at comments.
func nextLineOpensBlock(lines []string) bool {
	for _, line := range lines {
		line = strings.Trim(line, swayConfigWhitespace)
		if line != "" {
			return line == "{"
		}
	}
	return false
}

// Recognize only a trailing source token consisting of an unquoted brace.
// Escaped whitespace and quoted/bracketed payload remain part of their token;
// this framing check neither expands arguments nor interprets shell commands.
func hasTrailingBlockBrace(line string) bool {
	if !strings.HasSuffix(line, "{") {
		return false
	}
	start := 0
	var quote byte
	bracket, escaped := false, false
	for index := 0; index < len(line); index++ {
		character := line[index]
		if character == '\\' {
			escaped = !escaped
			continue
		}
		if !escaped {
			if quote != 0 {
				if character == quote {
					quote = 0
				}
			} else {
				switch character {
				case '\'', '"':
					quote = character
				case '[':
					bracket = true
				case ']':
					bracket = false
				case ' ', '\f', '\n', '\r', '\t', '\v':
					if !bracket {
						start = index + 1
					}
				}
			}
		}
		escaped = false
	}
	return start == len(line)-1
}

func literalIncludePath(argument string) (string, bool) {
	argument = strings.Trim(argument, swayConfigWhitespace)
	if argument == "" {
		return "", false
	}
	path := argument
	if argument[0] == '"' {
		var decoded strings.Builder
		closed := false
		for index := 1; index < len(argument); index++ {
			character := argument[index]
			if character == '"' {
				if strings.Trim(argument[index+1:], swayConfigWhitespace) != "" {
					return "", false
				}
				closed = true
				break
			}
			if character == '\\' {
				index++
				if index >= len(argument) || (argument[index] != '\\' && argument[index] != '"') {
					return "", false
				}
				character = argument[index]
			}
			decoded.WriteByte(character)
		}
		if !closed {
			return "", false
		}
		path = decoded.String()
	} else if strings.ContainsAny(argument, swayConfigWhitespace+"\\\"'") {
		return "", false
	}
	if path == "" || strings.ContainsAny(path, "\r\n\x00$`*?[") {
		return "", false
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return "", false
		}
	}
	return filepath.Clean(path), true
}
