package session

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TerminalRemoteBinding pins routing separately from the local context identity.
// MachineID records a saved profile's origin without making its mutable label
// or the global Herdr selection authoritative.
type TerminalRemoteBinding struct {
	Target    string `json:"target"`
	Session   string `json:"session"`
	MachineID string `json:"machine_id,omitempty"`
}

func (binding TerminalRemoteBinding) Validate() error {
	target := binding.Target
	if !utf8.ValidString(target) || len(target) == 0 || len(target) > 1024 || strings.HasPrefix(target, "-") || strings.ContainsAny(target, "\\\"'`;|&$<>(){}") || strings.ContainsFunc(target, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return errors.New("remote target must be a bounded SSH alias, user@host or ssh:// URL without credentials, whitespace or shell syntax")
	}
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return errors.New("remote target must be an SSH host URL without a path, query or fragment")
		}
		if u.User != nil {
			if _, password := u.User.Password(); password {
				return errors.New("remote target must not contain a password")
			}
		}
	} else if strings.ContainsAny(target, "/:?#%") || strings.Count(target, "@") > 1 || strings.HasPrefix(target, "@") || strings.HasSuffix(target, "@") {
		return errors.New("remote target must be an SSH alias or user@host; use ssh:// for a port")
	}
	if !validSessionName(binding.Session) {
		return errors.New("invalid remote Herdr session name")
	}
	if binding.MachineID != "" {
		if len(binding.MachineID) != 32 {
			return errors.New("invalid Herdr machine profile ID")
		}
		for _, c := range binding.MachineID {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return errors.New("invalid Herdr machine profile ID")
			}
		}
	}
	return nil
}

func IsRemoteTerminal(value Context) bool {
	return value.Launcher.Kind == LauncherHerdr && value.Launcher.Terminal != nil && value.Launcher.Terminal.Remote != nil
}

func sameTerminalRemote(left, right *TerminalRemoteBinding) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func cloneTerminalRemote(value *TerminalRemoteBinding) *TerminalRemoteBinding {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
