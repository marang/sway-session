package session

import (
	"cmp"
	"sort"
	"strings"
)

func boundedActionsAfter[T any](actions []T, limit int, after *T, compare func(T, T) int) []T {
	if len(actions) == 0 || limit <= 0 {
		return []T{}
	}
	sort.SliceStable(actions, func(left, right int) bool {
		return compare(actions[left], actions[right]) < 0
	})
	start := 0
	if after != nil {
		start = sort.Search(len(actions), func(index int) bool {
			return compare(actions[index], *after) > 0
		})
		if start == len(actions) {
			start = 0
		}
	}
	count := min(limit, len(actions))
	bounded := make([]T, 0, count)
	for offset := range count {
		bounded = append(bounded, actions[(start+offset)%len(actions)])
	}
	return bounded
}

func boundedPlacementActionsAfter(actions []PlacementAction, after *PlacementAction) []PlacementAction {
	if len(actions) == 0 {
		return []PlacementAction{}
	}
	sort.SliceStable(actions, func(left, right int) bool {
		return comparePlacementActions(actions[left], actions[right]) < 0
	})
	start := 0
	if after != nil {
		start = sort.Search(len(actions), func(index int) bool {
			return comparePlacementActions(actions[index], *after) > 0
		})
		if start == len(actions) {
			start = 0
		}
	}
	// Start at a context boundary even if a caller supplied a cursor between
	// that context's move and mark. Replaying an idempotent absolute move is
	// safer than marking a window whose placement was never confirmed.
	for start > 0 && actions[start-1].ContextID == actions[start].ContextID {
		start--
	}
	rotated := append(append(make([]PlacementAction, 0, len(actions)), actions[start:]...), actions[:start]...)
	bounded := make([]PlacementAction, 0, min(maxPlacementActions, len(actions)))
	for index := 0; index < len(rotated); {
		end := index + 1
		for end < len(rotated) && rotated[end].ContextID == rotated[index].ContextID {
			end++
		}
		if len(bounded)+(end-index) > maxPlacementActions {
			break
		}
		bounded = append(bounded, rotated[index:end]...)
		index = end
	}
	return bounded
}

func comparePlacementActions(left, right PlacementAction) int {
	if order := strings.Compare(string(left.ContextID), string(right.ContextID)); order != 0 {
		return order
	}
	if order := cmp.Compare(placementActionKindOrder(left.Kind), placementActionKindOrder(right.Kind)); order != 0 {
		return order
	}
	if order := cmp.Compare(left.ContainerID, right.ContainerID); order != 0 {
		return order
	}
	return strings.Compare(left.Workspace, right.Workspace)
}

func placementActionKindOrder(kind PlacementActionKind) int {
	if kind == PlacementMoveWorkspace {
		return 0
	}
	return 1
}

func boundedApplicationIndicatorActionsAfter(
	actions []ApplicationIndicatorAction,
	after *ApplicationIndicatorAction,
) []ApplicationIndicatorAction {
	return boundedActionsAfter(actions, maxApplicationIndicatorActions, after, compareApplicationIndicatorActions)
}

func compareApplicationIndicatorActions(left, right ApplicationIndicatorAction) int {
	if order := cmp.Compare(left.ContainerID, right.ContainerID); order != 0 {
		return order
	}
	if order := cmp.Compare(applicationIndicatorKindOrder(left.Kind), applicationIndicatorKindOrder(right.Kind)); order != 0 {
		return order
	}
	if order := strings.Compare(left.Mark, right.Mark); order != 0 {
		return order
	}
	return strings.Compare(string(left.State), string(right.State))
}

func applicationIndicatorKindOrder(kind ApplicationIndicatorActionKind) int {
	if kind == ApplicationIndicatorRemove {
		return 0
	}
	return 1
}
