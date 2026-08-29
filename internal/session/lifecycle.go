package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var (
	ErrContextNotFound  = errors.New("context not found")
	ErrContextAmbiguous = errors.New("context selector is ambiguous")
)

// NewContextID returns a random RFC 4122 version 4 UUID.
func NewContextID() (ContextID, error) {
	return newContextIDFrom(rand.Reader)
}

func newContextIDFrom(source io.Reader) (ContextID, error) {
	var value [16]byte
	if _, err := io.ReadFull(source, value[:]); err != nil {
		return "", fmt.Errorf("generate context ID: %w", err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return ContextID(fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16],
	)), nil
}

// ResolveContext returns the index selected by an exact UUID or exact label.
// Labels are presentation metadata and therefore must be unambiguous at use.
func ResolveContext(registry Registry, selector string) (int, error) {
	if selector == "" {
		return -1, fmt.Errorf("%w: selector is empty", ErrContextNotFound)
	}
	for index := range registry.Contexts {
		if string(registry.Contexts[index].ID) == selector {
			return index, nil
		}
	}
	match := -1
	for index := range registry.Contexts {
		if registry.Contexts[index].Label != selector {
			continue
		}
		if match >= 0 {
			return -1, fmt.Errorf("%w: label %q matches multiple contexts", ErrContextAmbiguous, selector)
		}
		match = index
	}
	if match < 0 {
		return -1, fmt.Errorf("%w: %q", ErrContextNotFound, selector)
	}
	return match, nil
}

func AddContext(registry *Registry, context Context) error {
	if registry == nil {
		return errors.New("context registry is nil")
	}
	if err := context.Validate(); err != nil {
		return err
	}
	if len(registry.Contexts) >= MaxContexts {
		return fmt.Errorf("context registry already contains the maximum of %d contexts", MaxContexts)
	}
	for index := range registry.Contexts {
		current := registry.Contexts[index]
		if current.ID == context.ID {
			return fmt.Errorf("context ID %q is already registered", context.ID)
		}
		if current.Launcher.identity() == context.Launcher.identity() {
			return fmt.Errorf("launcher session %q is already registered by context %q", context.Launcher.Session, current.ID)
		}
	}
	registry.Contexts = append(registry.Contexts, context)
	return registry.Validate()
}

func SetContextState(registry *Registry, selector string, state ContextState) (Context, error) {
	if registry == nil {
		return Context{}, errors.New("context registry is nil")
	}
	if state != ContextActive && state != ContextArchived {
		return Context{}, fmt.Errorf("unsupported context state %q", state)
	}
	index, err := ResolveContext(*registry, selector)
	if err != nil {
		return Context{}, err
	}
	registry.Contexts[index].State = state
	if err := registry.Validate(); err != nil {
		return Context{}, err
	}
	return registry.Contexts[index], nil
}

func RemoveContext(registry *Registry, selector string) (Context, error) {
	if registry == nil {
		return Context{}, errors.New("context registry is nil")
	}
	index, err := ResolveContext(*registry, selector)
	if err != nil {
		return Context{}, err
	}
	removed := registry.Contexts[index]
	copy(registry.Contexts[index:], registry.Contexts[index+1:])
	registry.Contexts = registry.Contexts[:len(registry.Contexts)-1]
	if err := registry.Validate(); err != nil {
		return Context{}, err
	}
	return removed, nil
}
