package naming

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PathProblem identifies why a logical relative path is invalid.
type PathProblem string

const (
	PathProblemEmpty     PathProblem = "empty"
	PathProblemAbsolute  PathProblem = "absolute"
	PathProblemTraversal PathProblem = "traversal"
	PathProblemTooLong   PathProblem = "too_long"
	PathProblemComponent PathProblem = "invalid_component"
	PathProblemReserved  PathProblem = "reserved_logical_name"
)

// PathValidationError reports a path-level problem without changing the path.
type PathValidationError struct {
	Problem        PathProblem
	ComponentIndex int
	Component      string
	Cause          error
}

func (e *PathValidationError) Error() string {
	if e.Component != "" {
		return fmt.Sprintf("invalid portable path at component %d %q: %s", e.ComponentIndex, e.Component, e.Problem)
	}
	return fmt.Sprintf("invalid portable path: %s", e.Problem)
}

func (e *PathValidationError) Unwrap() error { return e.Cause }

// ValidateRelativePath validates a logical slash-separated relative path.
// It does not reject internal reserved paths; normal user input should call
// ValidateUserRelativePath.
func ValidateRelativePath(relative string) error {
	if relative == "" {
		return &PathValidationError{Problem: PathProblemEmpty}
	}
	if !utf8.ValidString(relative) {
		return &PathValidationError{Problem: PathProblemComponent, Cause: &ValidationError{Problem: ProblemInvalidUTF8}}
	}
	if len(relative) > MaxRelativePathBytes {
		return &PathValidationError{Problem: PathProblemTooLong}
	}
	if strings.HasPrefix(relative, "/") {
		return &PathValidationError{Problem: PathProblemAbsolute}
	}

	parts := strings.Split(relative, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return &PathValidationError{
				Problem:        PathProblemTraversal,
				ComponentIndex: i,
				Component:      part,
			}
		}
		if err := ValidateComponent(part); err != nil {
			return &PathValidationError{
				Problem:        PathProblemComponent,
				ComponentIndex: i,
				Component:      part,
				Cause:          err,
			}
		}
	}
	return nil
}

// ValidateUserRelativePath additionally rejects logical names reserved for
// Remember itself.
func ValidateUserRelativePath(relative string) error {
	if err := ValidateRelativePath(relative); err != nil {
		return err
	}
	if index, reserved := reservedComponent(strings.Split(relative, "/")); reserved {
		parts := strings.Split(relative, "/")
		return &PathValidationError{
			Problem:        PathProblemReserved,
			ComponentIndex: index,
			Component:      parts[index],
		}
	}
	return nil
}

func reservedComponent(parts []string) (int, bool) {
	if len(parts) == 0 {
		return 0, false
	}
	root := CollisionKey(parts[0])
	if root == CollisionKey(".remember") || root == CollisionKey("_Konflikte") {
		return 0, true
	}
	if len(parts) > 1 && root == CollisionKey("_Konflikte") &&
		CollisionKey(parts[1]) == CollisionKey("Wiederhergestellt") {
		return 1, true
	}
	return 0, false
}
