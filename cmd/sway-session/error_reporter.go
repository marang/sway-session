package main

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/marang/sway-session/internal/diagnostic"
)

type diagnosticErrorReporter struct {
	mu          sync.Mutex
	writer      io.Writer
	structured  bool
	code        string
	message     string
	lastMessage string
	lastAt      time.Time
}

func newDiagnosticErrorReporter(
	writer io.Writer,
	structured bool,
	code string,
	message string,
) *diagnosticErrorReporter {
	return &diagnosticErrorReporter{
		writer: writer, structured: structured, code: code, message: message,
	}
}

func (reporter *diagnosticErrorReporter) Report(err error) {
	if reporter == nil || err == nil || reporter.writer == nil {
		return
	}
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	text := err.Error()
	if text == reporter.lastMessage && time.Since(reporter.lastAt) < 5*time.Second {
		return
	}
	reporter.lastMessage = text
	reporter.lastAt = time.Now()
	_ = diagnostic.WriteAll(reporter.writer, "sway-session", runtimeDiagnostics(err, reporter.code, reporter.message, text), reporter.structured)
}

type diagnosticProvider interface {
	Diagnostic() diagnostic.Diagnostic
}

func runtimeDiagnostics(err error, code string, message string, fallbackHint string) []diagnostic.Diagnostic {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		items := make([]diagnostic.Diagnostic, 0)
		for _, child := range joined.Unwrap() {
			items = append(items, runtimeDiagnostics(child, code, message, child.Error())...)
		}
		if len(items) != 0 {
			return items
		}
	}
	var provider diagnosticProvider
	if errors.As(err, &provider) {
		return []diagnostic.Diagnostic{provider.Diagnostic()}
	}
	return []diagnostic.Diagnostic{{
		Level: diagnostic.LevelError, Code: code, Message: message, Hint: fallbackHint,
	}}
}
