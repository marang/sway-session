package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

type swayLogicalLine struct {
	text  string
	start int
}

func forEachSwayLogicalLine(content []byte, visit func(swayLogicalLine) bool) error {
	physical := bufio.NewScanner(strings.NewReader(string(content)))
	physical.Buffer(make([]byte, 4096), maxSwayConfigLine)
	var logical strings.Builder
	var pending *swayLogicalLine
	var framingErr error
	start := 0
	line := 0
	continued := false
	emit := func(item swayLogicalLine) bool {
		trimmed := strings.TrimSpace(item.text)
		if pending != nil {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				return true
			}
			if trimmed == "{" {
				if len(pending.text)+2 > maxSwayConfigLine {
					framingErr = errors.New("block header exceeds the supported length")
					return false
				}
				pending.text += " {"
				ready := *pending
				pending = nil
				return visit(ready)
			}
			ready := *pending
			pending = nil
			if !visit(ready) {
				return false
			}
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasSuffix(trimmed, "{") || strings.HasSuffix(trimmed, "}") {
			return visit(item)
		}
		copy := item
		pending = &copy
		return true
	}
	for physical.Scan() {
		line++
		part := physical.Text()
		if start == 0 {
			start = line
		}
		isContinuation := strings.HasSuffix(part, "\\") && (len(part) == 0 || part[0] != '#')
		if isContinuation {
			part = strings.TrimSuffix(part, "\\")
		}
		if logical.Len()+len(part) > maxSwayConfigLine {
			return fmt.Errorf("continued line starting at %d exceeds the supported length", start)
		}
		logical.WriteString(part)
		continued = isContinuation
		if continued {
			continue
		}
		if !emit(swayLogicalLine{text: logical.String(), start: start}) {
			return framingErr
		}
		logical.Reset()
		start = 0
	}
	if err := physical.Err(); err != nil {
		return errors.New("a physical line exceeds the supported length")
	}
	if continued {
		return fmt.Errorf("continued line starting at %d has no following line", start)
	}
	if pending != nil {
		visit(*pending)
	}
	return nil
}

// lexSwayLine retains a completed prefix on failure. The classifier may ignore
// an argument error only when that prefix and the enclosing scope prove the
// declaration unrelated. Structural tokens are distinguished from quoted or
// escaped brace arguments; inline structural syntax is never silently skipped.
type swayLexedLine struct {
	tokens        []string
	starts        []int
	brace         byte
	embeddedBrace bool
	escaped       bool
	err           error
}

func lexSwayLine(line string) swayLexedLine {
	result := swayLexedLine{}
	if len(line) > maxSwayConfigLine {
		result.err = errors.New("logical line exceeds the supported length")
		return result
	}
	// Sway comments start at the first non-whitespace character. Hashes in
	// arguments, including hexadecimal colors and paths, remain literal.
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return result
	}
	var token strings.Builder
	var quote byte
	escaped, active, plain := false, false, true
	start := 0
	braces := 0
	finish := func() {
		if !active {
			return
		}
		value := token.String()
		result.tokens = append(result.tokens, value)
		result.starts = append(result.starts, start)
		result.brace = 0
		if plain && (value == "{" || value == "}") {
			result.brace = value[0]
			braces++
		}
		token.Reset()
		active, plain = false, true
	}
	for index := 0; index < len(line); index++ {
		character := line[index]
		if !active {
			start = index
		}
		if character == 0 {
			result.err = errors.New("NUL in logical line")
			break
		}
		if escaped {
			token.WriteByte(character)
			active = true
			escaped = false
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped, active, plain = true, true, false
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				token.WriteByte(character)
			}
			active = true
			continue
		}
		switch character {
		case '\'', '"':
			quote, active, plain = character, true, false
		case ' ', '\t', '\r':
			finish()
		default:
			token.WriteByte(character)
			active = true
		}
	}
	finish()
	result.embeddedBrace = braces > 1 || (braces != 0 && result.brace == 0)
	if escaped {
		result.escaped = true
		result.err = errors.New("unfinished escape")
	}
	if quote != 0 {
		result.err = errors.New("unterminated quoted string")
	}
	return result
}
func tokenizeSwayLine(line string) ([]string, bool, error) {
	lexed := lexSwayLine(line)
	if lexed.escaped {
		return lexed.tokens, true, nil
	}
	return lexed.tokens, false, lexed.err
}

func swayExecPayload(raw string) string {
	lexed := lexSwayLine(raw)
	if len(lexed.tokens) < 2 || (!strings.EqualFold(lexed.tokens[0], "exec") && !strings.EqualFold(lexed.tokens[0], "exec_always")) {
		return ""
	}
	index := 1
	if lexed.tokens[index] == "--no-startup-id" {
		index++
	}
	if index == len(lexed.tokens) {
		return ""
	}
	return raw[lexed.starts[index]:]
}
