// Package buildmetadata identifies builds without executing another binary or
// consulting configuration, session state, runtime sockets, or the checkout.
package buildmetadata

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const stampPrefix = "sway-session-build-v1|"
const stampSuffix = "|end-sway-session-build-v1"
const maxStampSize = 256
const maxExecutableSize = 256 << 20

// Stamp is populated by -X. Unlike recorded -ldflags, this referenced string
// survives -trimpath, -s -w and PIE. Empty means no dedicated build stamp.
var Stamp string

type Metadata struct {
	Version  string `json:"product_version"`
	Commit   string `json:"commit"`
	Modified bool   `json:"modified"`
}

func unknown() Metadata { return Metadata{Version: "unknown", Commit: "unknown"} }

func Current() Metadata {
	if value, ok := parseStamp(Stamp); ok {
		return value
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unknown()
	}
	value := Read(info)
	if value.Version == "unknown" {
		value.Version = "dev"
	}
	return value
}

// Executing preserves legacy main.version linker overrides for the CLI. New
// stamped builds share Current's immutable identity with executable readers.
func Executing(version, commit, modified string) Metadata {
	if value, ok := parseStamp(Stamp); ok {
		return value
	}
	value := Current()
	if validVersion(version) {
		value.Version = version
	}
	if validCommit(commit) && commit != "unknown" {
		value.Commit = commit
	}
	if modified == "true" || modified == "false" {
		value.Modified = modified == "true"
	}
	return value
}

// Read uses recorded flags only when available; Go omits them with -trimpath.
// VCS settings and a published main module version provide older-build fallback.
func Read(info *debug.BuildInfo) Metadata {
	value := unknown()
	if info == nil {
		return value
	}
	if info.Main.Version != "(devel)" && validVersion(info.Main.Version) {
		value.Version = info.Main.Version
	}
	flags := ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if validCommit(setting.Value) {
				value.Commit = setting.Value
			}
		case "vcs.modified":
			value.Modified = setting.Value == "true"
		case "-ldflags":
			flags = setting.Value
		}
	}
	words, ok := splitFlags(flags)
	if !ok {
		return value
	}
	for index := 0; index < len(words); index++ {
		word := words[index]
		if word == "-X" {
			index++
			if index >= len(words) {
				break
			}
			word = words[index]
		} else if strings.HasPrefix(word, "-X=") {
			word = strings.TrimPrefix(word, "-X=")
		} else {
			continue
		}
		name, text, found := strings.Cut(word, "=")
		if !found {
			continue
		}
		switch name {
		case "github.com/marang/sway-session/internal/buildmetadata.Stamp":
			if stamped, ok := parseStamp(text); ok {
				return stamped
			}
		case "main.version":
			if validVersion(text) {
				value.Version = text
			}
		case "main.commit":
			if validCommit(text) {
				value.Commit = text
			}
		case "main.modified":
			if text == "true" || text == "false" {
				value.Modified = text == "true"
			}
		}
	}
	return value
}

// ReadFile pins one file descriptor and reads its build information and stamp.
// /proc/PID/exe can identify a still-running, replaced executable. No execution
// or lookup of a newer binary on PATH is performed. Conflicting stamps fail.
func ReadFile(path string) (Metadata, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return unknown(), err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	return ReadExecutable(context.Background(), file)
}

// ReadExecutable reads the already pinned regular executable descriptor. It
// leaves its seek position unchanged and checks cancellation between chunks.
func ReadExecutable(ctx context.Context, file *os.File) (Metadata, error) {
	if err := ctx.Err(); err != nil {
		return unknown(), err
	}
	if file == nil {
		return unknown(), errors.New("missing executable descriptor")
	}
	stat, err := file.Stat()
	if err != nil {
		return unknown(), err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > maxExecutableSize {
		return unknown(), errors.New("executable is not a bounded regular file")
	}
	info, err := buildinfo.Read(file)
	if err != nil {
		return unknown(), fmt.Errorf("read executable build information: %w", err)
	}
	stamped, found, err := readStamp(ctx, io.NewSectionReader(file, 0, stat.Size()))
	if err != nil {
		return unknown(), err
	}
	if found {
		return stamped, nil
	}
	return Read(info), nil
}

func parseStamp(text string) (Metadata, bool) {
	if len(text) > maxStampSize || !strings.HasPrefix(text, stampPrefix) || !strings.HasSuffix(text, stampSuffix) {
		return Metadata{}, false
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(text, stampPrefix), stampSuffix), "|")
	if len(fields) != 3 || !validVersion(fields[0]) || !validCommit(fields[1]) || (fields[2] != "true" && fields[2] != "false") {
		return Metadata{}, false
	}
	return Metadata{Version: fields[0], Commit: fields[1], Modified: fields[2] == "true"}, true
}

func readStamp(ctx context.Context, reader io.Reader) (Metadata, bool, error) {
	var value Metadata
	found := false
	buffer := make([]byte, 64<<10)
	tail := []byte(nil)
	for {
		if err := ctx.Err(); err != nil {
			return unknown(), false, err
		}
		n, err := reader.Read(buffer)
		data := append(tail, buffer[:n]...)
		remaining := data
		for {
			start := bytes.Index(remaining, []byte(stampPrefix))
			if start < 0 {
				break
			}
			candidate := remaining[start:]
			candidate = candidate[:min(len(candidate), maxStampSize)]
			end := bytes.Index(candidate, []byte(stampSuffix))
			if end >= 0 {
				if next, valid := parseStamp(string(candidate[:end+len(stampSuffix)])); valid {
					if found && next != value {
						return unknown(), false, errors.New("executable contains conflicting build stamps")
					}
					value, found = next, true
				}
			}
			remaining = remaining[start+len(stampPrefix):]
		}
		tail = append([]byte(nil), data[max(len(data)-maxStampSize, 0):]...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return unknown(), false, err
		}
	}
	return value, found, nil
}

func validVersion(text string) bool {
	if text == "" || len(text) > 96 {
		return false
	}
	return strings.Trim(text, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.+_- ") == "" && !strings.Contains(text, " ")
}

func validCommit(text string) bool {
	return text == "unknown" || (len(text) == 40 && strings.Trim(text, "0123456789abcdef") == "")
}

// The Go linker uses whitespace-separated arguments with whole-field quotes;
// there is no shell evaluation or backslash unescaping inside quoted fields.
func splitFlags(text string) ([]string, bool) {
	if len(text) > 64<<10 {
		return nil, false
	}
	var words []string
	for text != "" {
		text = strings.TrimLeft(text, " \t\r\n")
		if text == "" {
			break
		}
		if text[0] == '\'' || text[0] == '"' {
			quote := text[0]
			text = text[1:]
			end := strings.IndexByte(text, quote)
			if end < 0 {
				return nil, false
			}
			words = append(words, text[:end])
			text = text[end+1:]
		} else {
			end := strings.IndexAny(text, " \t\r\n")
			if end < 0 {
				words = append(words, text)
				break
			}
			words = append(words, text[:end])
			text = text[end:]
		}
	}
	return words, true
}
