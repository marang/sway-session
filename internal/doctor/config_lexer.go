package doctor

import (
	"path/filepath"
	"strings"
)

// literalDirectInclude finds only an ordinary, top-level literal include in
// the selected main file. It does not expand variables, follow includes, parse
// shell commands, or infer whether Sway loaded this source.
func literalDirectInclude(content []byte, main, snippet string) int {
	depth := 0
	var logical strings.Builder
	start, found := 1, 0
	continued, oversized := false, false
	for index, physical := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(physical)
		comment := strings.HasPrefix(trimmed, "#")
		continuation := !comment && strings.HasSuffix(physical, "\\")
		part := physical
		if continuation {
			part = strings.TrimSuffix(part, "\\")
		}
		if logical.Len()+len(part) > maxSwayConfigLine {
			oversized = true
		}
		if !oversized {
			logical.WriteString(part)
		}
		if continuation {
			continued = true
			continue
		}
		if !oversized {
			line := strings.TrimSpace(logical.String())
			if line != "" && !strings.HasPrefix(line, "#") {
				fields := strings.Fields(line)
				command := strings.ToLower(fields[0])
				if depth == 0 && !continued && command == "include" {
					if path, ok := literalIncludePath(line[len(fields[0]):]); ok {
						if !filepath.IsAbs(path) {
							path = filepath.Join(filepath.Dir(main), path)
						}
						if path == snippet && found == 0 {
							found = start
						}
					}
				}
				// Command payloads are opaque. In particular, shell braces in
				// unrelated startup commands do not change source scope.
				if command != "exec" && command != "exec_always" {
					if line == "}" {
						if depth > 0 {
							depth--
						}
					} else if fields[len(fields)-1] == "{" {
						depth++
					}
				}
			}
		}
		logical.Reset()
		continued, oversized = false, false
		start = index + 2
		if depth > maxSwayBlockDepth {
			// The source is beyond this bounded recognizer. Any earlier
			// evidence remains source evidence, without a load-order claim.
			return found
		}
	}
	return found
}

func literalIncludePath(argument string) (string, bool) {
	argument = strings.TrimSpace(argument)
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
				if strings.TrimSpace(argument[index+1:]) != "" {
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
	} else if strings.ContainsAny(argument, " \t\r\n\\\"'") {
		return "", false
	}
	if path == "" || strings.ContainsAny(path, "\r\n\x00$`*?[") || filepath.Clean(path) != path {
		return "", false
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return "", false
		}
	}
	return path, true
}
