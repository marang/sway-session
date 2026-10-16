package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// DesktopApprovalOptions records an explicit registration/reapproval decision.
// It never authorizes a command supplied through a session broker.
type DesktopApprovalOptions struct {
	AllowAdministrative bool
}

// AdministrativeDesktopApprovalDigest binds a pending registration decision
// to the complete current trust identity. It creates no approval snapshot.
func AdministrativeDesktopApprovalDigest(ctx context.Context, entry DesktopEntry) (string, error) {
	if ctx == nil {
		return "", errors.New("administrative approval context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, _, parsed, err := revalidateCatalogDesktopEntry(entry)
	if err != nil {
		return "", err
	}
	approval, err := prepareAdministrativeApproval(ctx, data, parsed)
	if err != nil {
		return "", err
	}
	return approval.Digest()
}

// AdministrativeDesktopApproval pins the reviewed desktop entry and both
// root-owned executables. No arguments or authentication data are persisted.
type AdministrativeDesktopApproval struct {
	DesktopEntrySHA256 string `json:"desktop_entry_sha256"`
	HelperPath         string `json:"helper_path"`
	HelperSHA256       string `json:"helper_sha256"`
	ExecutablePath     string `json:"executable_path"`
	ExecutableSHA256   string `json:"executable_sha256"`
}

// Digest binds one pending decision to all reviewed launch trust material.
func (approval *AdministrativeDesktopApproval) Digest() (string, error) {
	if approval == nil {
		return "", errors.New("administrative approval is nil")
	}
	if err := approval.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(approval)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}

func (approval *AdministrativeDesktopApproval) validate() error {
	if approval == nil {
		return nil
	}
	if approval.HelperPath != "/usr/bin/pkexec" && approval.HelperPath != "/usr/bin/sudo" {
		return errors.New("administrative helper must be /usr/bin/pkexec or /usr/bin/sudo")
	}
	if err := validateAbsoluteFilePath("administrative executable", approval.ExecutablePath, ""); err != nil {
		return err
	}
	if isIndirectOrPrivilegedExecutable(approval.ExecutablePath) {
		return errors.New("administrative payload must not be an indirect or administrative executable")
	}
	for name, digest := range map[string]string{
		"administrative desktop entry": approval.DesktopEntrySHA256,
		"administrative helper":        approval.HelperSHA256,
		"administrative executable":    approval.ExecutableSHA256,
	} {
		if err := validateSHA256(name, digest); err != nil {
			return err
		}
	}
	return nil
}

type administrativeDesktopShape struct {
	helperPath string
	executable string
	arguments  []string
}

// This recognizes two explicit helper grammars, not every application which
// might request privileges internally. Only approved desktop bytes supply argv.
func administrativeDesktopEntry(entry DesktopEntry) (administrativeDesktopShape, bool, error) {
	if entry.Exec == "" {
		return administrativeDesktopShape{}, false, nil
	}
	first, err := desktopExecExecutable(entry.Exec)
	if err != nil {
		return administrativeDesktopShape{}, false, err
	}
	name := filepath.Base(first)
	if name != "pkexec" && name != "sudo" {
		return administrativeDesktopShape{}, false, nil
	}
	if entry.Terminal || entry.DBusActivatable || entry.FlatpakID != "" || entry.WorkingDirectory != "" {
		return administrativeDesktopShape{}, true, errors.New("administrative starter requires direct desktop execution without Terminal, D-Bus activation or a Path working directory")
	}
	helper := "/usr/bin/" + name
	if first != name && first != helper {
		return administrativeDesktopShape{}, true, errors.New("administrative helper must name its /usr/bin executable")
	}
	words, err := parseDesktopExecWords(entry.Exec, true)
	if err != nil {
		return administrativeDesktopShape{}, true, err
	}
	payload := 1
	if name == "sudo" && len(words) > payload && (words[payload] == "-A" || words[payload] == "--askpass") {
		payload++
	}
	if len(words) <= payload {
		return administrativeDesktopShape{}, true, errors.New("administrative starter requires an absolute payload executable")
	}
	executable := words[payload]
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable ||
		strings.Contains(executable, "%") || containsControl(executable) ||
		isIndirectOrPrivilegedExecutable(executable) {
		return administrativeDesktopShape{}, true, errors.New("administrative starter requires a clean absolute direct payload; helper options and indirect payloads are unsupported")
	}
	shape := administrativeDesktopShape{helperPath: helper, executable: executable, arguments: words[1:]}
	// Validate expansion at approval too, before accepting an unsupported form.
	if _, err := administrativeDesktopArguments(shape, entry, entry.Path); err != nil {
		return administrativeDesktopShape{}, true, err
	}
	return shape, true, nil
}

func validateDesktopApprovalOptions(entry DesktopEntry, options DesktopApprovalOptions) (bool, error) {
	_, administrative, err := administrativeDesktopEntry(entry)
	if err != nil {
		return false, err
	}
	if administrative {
		if !options.AllowAdministrative {
			return false, errors.New("administrative desktop starter requires explicit --allow-administrative approval; restoration may request authentication")
		}
		return true, nil
	}
	return false, rejectAdministrativeDesktopEntry(entry)
}

func prepareAdministrativeApproval(ctx context.Context, data []byte, entry DesktopEntry) (*AdministrativeDesktopApproval, error) {
	shape, administrative, err := administrativeDesktopEntry(entry)
	if err != nil || !administrative {
		if err == nil {
			err = errors.New("approved administrative desktop entry no longer has its helper")
		}
		return nil, err
	}

	helperDigest, err := hashTrustedExecutableContext(ctx, shape.helperPath, 0, true)
	if err != nil {
		return nil, fmt.Errorf("validate administrative helper: %w", err)
	}
	executableDigest, err := hashTrustedExecutableContext(ctx, shape.executable, 0, true)
	if err != nil {
		return nil, fmt.Errorf("validate administrative executable: %w", err)
	}
	return &AdministrativeDesktopApproval{
		DesktopEntrySHA256: sha256Hex(data),
		HelperPath:         shape.helperPath, HelperSHA256: helperDigest,
		ExecutablePath: shape.executable, ExecutableSHA256: executableDigest,
	}, nil
}

func revalidateAdministrativeApproval(ctx context.Context, launcher Launcher, data []byte, entry DesktopEntry) error {
	expected := launcher.Administrative
	if expected == nil {
		return errors.New("administrative approval is missing")
	}
	current, err := prepareAdministrativeApproval(ctx, data, entry)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return launcherValidationFailure(ApplicationLauncherReapprovalRequired, "administrative_approval_changed",
			errors.New("administrative launcher trust changed and requires explicit reapproval"))
	}
	if *expected != *current {
		return launcherValidationFailure(ApplicationLauncherReapprovalRequired, "administrative_approval_changed",
			errors.New("administrative desktop entry, helper or executable changed and requires explicit reapproval"))
	}
	return nil
}

// desktopProcessSpec uses a persistent daemon parent for authentication helpers.
// GIO exits before pkexec authenticates, which can make pkexec reject its dead
// parent. Ordinary desktop entries keep the existing GIO launch contract.
func desktopProcessSpec(launcher Launcher, entry DesktopEntry, path string) (ProcessSpec, error) {
	spec := ProcessSpec{Environment: []string{"PATH=/usr/local/bin:/usr/bin"}}
	if launcher.Administrative == nil {
		spec.Name, spec.Arguments = systemGIOExecutable, []string{"launch", path}
		return spec, nil
	}
	shape, administrative, err := administrativeDesktopEntry(entry)
	if err != nil || !administrative {
		return ProcessSpec{}, errors.New("approved administrative starter is invalid")
	}
	spec.Name = shape.helperPath
	spec.Arguments, err = administrativeDesktopArguments(shape, entry, path)
	return spec, err
}

// Expand only the bounded subset reviewed at registration. Replacement values
// stay single argv elements, never reparse as fields, words or shell syntax.
// No file/URI launch inputs are supplied by session restore.
func administrativeDesktopArguments(shape administrativeDesktopShape, entry DesktopEntry, desktopPath string) ([]string, error) {
	const maxArguments, maxBytes = 256, 64 * 1024
	var result []string
	size := 0
	appendArgument := func(value string) error {
		size += len(value) + 1
		if len(result) >= maxArguments || size > maxBytes || containsControl(value) {
			return errors.New("administrative desktop arguments exceed bounds or contain controls")
		}
		result = append(result, value)
		return nil
	}
	for _, word := range shape.arguments {
		var values []string
		switch word {
		case "%f", "%F", "%u", "%U":
			continue
		case "%c":
			values = []string{entry.Name}
		case "%k":
			values = []string{desktopPath}
		case "%i":
			if entry.Icon != "" {
				values = []string{"--icon", entry.Icon}
			}
		default:
			var literal strings.Builder
			for i := 0; i < len(word); i++ {
				if word[i] == '%' {
					if i+1 >= len(word) || word[i+1] != '%' {
						return nil, errors.New("administrative desktop arguments support standalone %f/%F/%u/%U/%c/%k/%i and literal %% only")
					}
					i++
				}
				literal.WriteByte(word[i])
			}
			values = []string{literal.String()}
		}
		for _, value := range values {
			if err := appendArgument(value); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
