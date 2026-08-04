package naming

import (
	"fmt"
	"sort"
)

// Collision contains all sibling names sharing one portable comparison key.
type Collision struct {
	Key   string
	Names []string
}

// SiblingValidationError identifies an invalid input name by position.
type SiblingValidationError struct {
	Index int
	Name  string
	Cause error
}

func (e *SiblingValidationError) Error() string {
	return fmt.Sprintf("invalid sibling name at index %d %q: %v", e.Index, e.Name, e.Cause)
}

func (e *SiblingValidationError) Unwrap() error { return e.Cause }

// FindSiblingCollisions validates names and returns every case-insensitive
// collision without changing or dropping any input name.
func FindSiblingCollisions(names []string) ([]Collision, error) {
	groups := make(map[string][]string, len(names))
	for i, name := range names {
		if err := ValidateComponent(name); err != nil {
			return nil, &SiblingValidationError{Index: i, Name: name, Cause: err}
		}
		key := CollisionKey(name)
		groups[key] = append(groups[key], name)
	}

	collisions := make([]Collision, 0)
	for key, group := range groups {
		if len(group) < 2 {
			continue
		}
		collisions = append(collisions, Collision{Key: key, Names: append([]string(nil), group...)})
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i].Key < collisions[j].Key })
	return collisions, nil
}
