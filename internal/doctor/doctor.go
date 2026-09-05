// Package doctor diagnoses sway-session setup and prepares explicit, narrow
// configuration repairs. Inspection never creates runtime or session state.
package doctor

import "context"

type Status string

const (
	OK          Status = "ok"
	Warning     Status = "warning"
	Error       Status = "error"
	Unavailable Status = "unavailable"
)

type Check struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Status   Status   `json:"status"`
	Detail   string   `json:"detail"`
	Hint     string   `json:"hint,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
	FixID    string   `json:"fix_id,omitempty"`
}

type Report struct {
	Checks []Check `json:"checks"`
}

func (report Report) HasErrors() bool {
	for _, check := range report.Checks {
		if check.Status == Error {
			return true
		}
	}
	return false
}

// Options selects existing configuration or IPC endpoints, never commands.
// Empty fields use the same XDG/Sway defaults as the ordinary CLI.
type Options struct {
	ConfigPath     string
	SwayConfigPath string
	Socket         string
	Executable     string
}

type Service struct {
	options Options
}

func New(options Options) *Service { return &Service{options: options} }

func (service *Service) Check(ctx context.Context) Report {
	checks := inspectRuntime(ctx, service.options)
	checks = append(checks, inspectSwayConfig(ctx, service.options)...)
	// Optional hardening follows all runtime and Sway setup checks so the TUI
	// can render it as a distinct final section.
	checks = append(checks, appArmorAvailabilityCheck())
	return Report{Checks: checks}
}

// Plan is a preview for one known repair. Private edit data is created by
// Service.Plan and must be revalidated before Service.Apply writes anything.
type Plan struct {
	ID      string       `json:"id"`
	Summary string       `json:"summary"`
	Changes []FileChange `json:"changes"`
	edits   []fileEdit
}

type FileChange struct {
	Path    string `json:"path"`
	Preview string `json:"preview"`
}

type FixResult struct {
	ID      string   `json:"id"`
	Message string   `json:"message"`
	Backups []string `json:"backups,omitempty"`
}
