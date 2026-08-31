package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestDiagnosticErrorReporterSerializesAndDeduplicatesConcurrentFailures(t *testing.T) {
	var output bytes.Buffer
	reporter := newDiagnosticErrorReporter(&output, false, "daemon_runtime", "persistent session daemon")
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			reporter.Report(errors.New("same failure"))
		}()
	}
	wait.Wait()
	if count := strings.Count(output.String(), "same failure"); count != 1 {
		t.Fatalf("concurrent duplicate failure count = %d, want 1: %q", count, output.String())
	}
}
