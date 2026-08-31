package session

import (
	"errors"
	"fmt"
	"strings"
)

type ApprovalChoice struct {
	Label string
	Token string
}

type SwaynagApprovalPresenter struct {
	Swaynag     string
	SwaySession string
	Starter     ProcessStarter
}

// Present opens one native Sway confirmation. The action is a fixed root-owned
// sway-session command plus a validated random token; no registry or desktop
// entry value becomes shell syntax.
func (presenter SwaynagApprovalPresenter) Present(message string, choices []ApprovalChoice) error {
	if presenter.Starter == nil {
		return errors.New("process starter is nil")
	}
	if presenter.Swaynag != "/usr/bin/swaynag" || presenter.SwaySession != "/usr/bin/sway-session" {
		return errors.New("swaynag presenter requires fixed /usr/bin/swaynag and /usr/bin/sway-session executables")
	}
	if message == "" || len(message) > 4096 || containsControl(message) {
		return errors.New("approval message must be bounded and contain no control characters")
	}
	if len(choices) == 0 || len(choices) > 32 {
		return errors.New("approval must contain between 1 and 32 choices")
	}
	arguments := []string{"-t", "warning", "-m", message}
	for _, choice := range choices {
		if choice.Label == "" || len(choice.Label) > 256 || containsControl(choice.Label) || !validOperationToken(choice.Token) {
			return errors.New("approval choice is invalid")
		}
		action := fmt.Sprintf("%s app confirm %s", presenter.SwaySession, choice.Token)
		if strings.ContainsAny(action, "\r\n") {
			return errors.New("approval action is invalid")
		}
		arguments = append(arguments, "-b", choice.Label, action)
	}
	return presenter.Starter.Start(ProcessSpec{Name: presenter.Swaynag, Arguments: arguments})
}
