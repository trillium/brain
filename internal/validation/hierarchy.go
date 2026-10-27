package validation

import (
	"fmt"
	"strings"
)

const (
	// HierarchyPriorityKey is the per-store opt-in for the specificity ladder.
	// The hierarchy. namespace is yaml-backed, so bd config set writes this
	// key to config.yaml rather than the SQL config table.
	HierarchyPriorityKey = "hierarchy.priority"

	// HierarchyPrioritySpecificity turns the ladder on for one store.
	// Any other value, including empty, leaves priority behavior unchanged.
	HierarchyPrioritySpecificity = "specificity"
)

// HierarchySpecificityEnabled reports whether value opts a store into the
// specificity ladder. The match is exact after trimming space; "Specificity"
// does not enable the gate.
func HierarchySpecificityEnabled(value string) bool {
	return strings.TrimSpace(value) == HierarchyPrioritySpecificity
}

// ChildMayFollowParent reports whether a parent-child edge is legal under the
// specificity ladder. The child priority must be strictly greater than the
// parent priority. The one exception is a parent at 4, which may have a child
// at 4. Skips are legal: a parent at 0 may have a child at 4.
func ChildMayFollowParent(parentPriority, childPriority int) bool {
	if parentPriority == 4 && childPriority == 4 {
		return true
	}
	return childPriority > parentPriority
}

// HierarchySpecificityError is the rejection for an illegal parent-child edge.
// The text names both ids and both priority numbers.
func HierarchySpecificityError(childID string, childPriority int, parentID string, parentPriority int) error {
	return fmt.Errorf("child %s priority %d is not more specific than parent %s priority %d",
		childID, childPriority, parentID, parentPriority)
}
