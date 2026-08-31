package main

import (
	"io"
	"sync"
	"time"

	"github.com/marang/sway-title-animator/internal/diagnostic"
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
	_ = diagnostic.WriteAll(reporter.writer, "sway-session", []diagnostic.Diagnostic{{
		Level: diagnostic.LevelError, Code: reporter.code, Message: reporter.message, Hint: text,
	}}, reporter.structured)
}
