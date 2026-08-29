// Package diagnostic renders stable human and machine-readable command diagnostics.
package diagnostic

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

type Level string

const LevelError Level = "error"

// Diagnostic is one actionable, non-sensitive command result.
type Diagnostic struct {
	Level   Level  `json:"level"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type envelope struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Write renders a diagnostic without terminal control sequences. JSON mode is
// stable for automation; text mode keeps the recovery hint on a separate line.
func Write(writer io.Writer, application string, item Diagnostic, structured bool) error {
	return WriteAll(writer, application, []Diagnostic{item}, structured)
}

// WriteAll renders one JSON envelope for a complete operation while retaining
// the same line-oriented human format as Write.
func WriteAll(writer io.Writer, application string, items []Diagnostic, structured bool) error {
	if structured {
		return json.NewEncoder(writer).Encode(envelope{Diagnostics: items})
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(
			writer,
			"%s: %s: %s\n",
			escapeControls(application),
			escapeControls(string(item.Level)),
			escapeControls(item.Message),
		); err != nil {
			return err
		}
		if item.Hint != "" {
			if _, err := fmt.Fprintf(writer, "%s: hint: %s\n", escapeControls(application), escapeControls(item.Hint)); err != nil {
				return err
			}
		}
	}
	return nil
}

func escapeControls(value string) string {
	var escaped strings.Builder
	for _, character := range value {
		if !unicode.IsControl(character) {
			escaped.WriteRune(character)
			continue
		}
		quoted := strconv.QuoteRune(character)
		escaped.WriteString(quoted[1 : len(quoted)-1])
	}
	return escaped.String()
}
