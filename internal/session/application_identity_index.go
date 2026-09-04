package session

import "fmt"

type applicationContextIndexGroup struct {
	wildcard  int
	sandboxes map[string]int
}

type applicationContextIndex struct {
	registry *Registry
	byID     map[ContextID]int
	groups   map[applicationPrimaryIdentity]applicationContextIndexGroup
}

func buildApplicationContextIndex(registry *Registry, activeOnly bool) applicationContextIndex {
	index := applicationContextIndex{
		registry: registry,
		byID:     make(map[ContextID]int, len(registry.Contexts)),
		groups:   make(map[applicationPrimaryIdentity]applicationContextIndexGroup, len(registry.Contexts)),
	}
	for contextIndex := range registry.Contexts {
		contextValue := &registry.Contexts[contextIndex]
		if !activeOnly {
			index.byID[contextValue.ID] = contextIndex
		}
		if contextValue.App == nil || activeOnly && contextValue.State != ContextActive {
			continue
		}
		if activeOnly {
			index.byID[contextValue.ID] = contextIndex
		}
		identity := contextValue.App.Identity
		primary := identity.primary()
		group, exists := index.groups[primary]
		if !exists {
			group = applicationContextIndexGroup{wildcard: -1, sandboxes: make(map[string]int)}
		}
		if identity.SandboxAppID == "" {
			group.wildcard = contextIndex
		} else {
			group.sandboxes[identity.SandboxAppID] = contextIndex
		}
		index.groups[primary] = group
	}
	return index
}

func (index applicationContextIndex) contextByID(id ContextID) (*Context, bool) {
	contextIndex, exists := index.byID[id]
	if !exists {
		return nil, false
	}
	return &index.registry.Contexts[contextIndex], true
}

func (index applicationContextIndex) match(identity ApplicationIdentity) (*Context, bool, error) {
	group, exists := index.groups[identity.primary()]
	if !exists {
		return nil, false, nil
	}
	if group.wildcard >= 0 {
		return &index.registry.Contexts[group.wildcard], true, nil
	}
	if identity.SandboxAppID != "" {
		contextIndex, exists := group.sandboxes[identity.SandboxAppID]
		if !exists {
			return nil, false, nil
		}
		return &index.registry.Contexts[contextIndex], true, nil
	}
	if len(group.sandboxes) == 1 {
		for _, contextIndex := range group.sandboxes {
			return &index.registry.Contexts[contextIndex], true, nil
		}
	}
	return nil, false, fmt.Errorf("application identity without a sandbox ID overlaps %d registered contexts", len(group.sandboxes))
}
