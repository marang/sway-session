package main

// TerminalCloseGuard is the shared shutdown guard for automatic terminal
// archival and Follow application closes. Snapshot must use only memory; a
// missing or unsafe guard preserves restore eligibility.
type TerminalCloseGuard interface {
	Snapshot() (uint64, bool)
}

type automaticCloseGeneration struct {
	shutdown    uint64
	eventStream uint64
}

// automaticCloseObservation binds a tree to the guard state before its IPC
// acquisition. Neither a later planning pass nor a successful rearm may treat
// that tree as presence observed in a different healthy generation.
type automaticCloseObservation struct {
	generation automaticCloseGeneration
	safe       bool
}

func (runtime *sessionRuntime) automaticCloseObservation() automaticCloseObservation {
	generation, safe := runtime.automaticCloseSnapshot()
	return automaticCloseObservation{generation: generation, safe: safe}
}

func (runtime *sessionRuntime) automaticCloseSnapshot() (automaticCloseGeneration, bool) {
	if runtime == nil || runtime.shutdown || runtime.context().Err() != nil || runtime.terminalCloseGuard == nil {
		return automaticCloseGeneration{}, false
	}
	generation, safe := runtime.terminalCloseGuard.Snapshot()
	return automaticCloseGeneration{shutdown: generation, eventStream: runtime.eventStreamEpoch},
		safe && runtime.terminalCloseStreamCurrent()
}
