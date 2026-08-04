package naming

import (
	"errors"
	"strings"
	"testing"
)

func TestComponentLengthBoundaries(t *testing.T) {
	t.Parallel()

	if err := ValidateComponent(strings.Repeat("a", MaxComponentBytes)); err != nil {
		t.Fatalf("exact component limit rejected: %v", err)
	}

	err := ValidateComponent(strings.Repeat("a", MaxComponentBytes+1))
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Problem != ProblemTooLong {
		t.Fatalf("overlong component error = %v, want ProblemTooLong", err)
	}
}

func TestValidateRelativePath(t *testing.T) {
	t.Parallel()

	for _, valid := range []string{
		"Note.md",
		"Folder/Note.md",
		"a/b/c/日本語.md",
		"_Konflikte/Wiederhergestellt/Note.md",
	} {
		if err := ValidateRelativePath(valid); err != nil {
			t.Errorf("ValidateRelativePath(%q) unexpected error: %v", valid, err)
		}
	}

	tests := []struct {
		path    string
		problem PathProblem
	}{
		{path: "", problem: PathProblemEmpty},
		{path: "/absolute.md", problem: PathProblemAbsolute},
		{path: "../escape.md", problem: PathProblemTraversal},
		{path: "folder/./note.md", problem: PathProblemTraversal},
		{path: "folder//note.md", problem: PathProblemTraversal},
		{path: "folder/CON.md", problem: PathProblemComponent},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := ValidateRelativePath(tt.path)
			var pathErr *PathValidationError
			if !errors.As(err, &pathErr) || pathErr.Problem != tt.problem {
				t.Errorf("ValidateRelativePath(%q) error = %v, want %q", tt.path, err, tt.problem)
			}
		})
	}
}

func TestRelativePathLengthBoundary(t *testing.T) {
	t.Parallel()

	parts := []string{
		strings.Repeat("a", 153),
		strings.Repeat("b", 153),
		strings.Repeat("c", 153),
		strings.Repeat("d", 153),
		strings.Repeat("e", 152),
	}
	exact := strings.Join(parts, "/")
	if len(exact) != MaxRelativePathBytes {
		t.Fatalf("test setup path length = %d, want %d", len(exact), MaxRelativePathBytes)
	}
	if err := ValidateRelativePath(exact); err != nil {
		t.Fatalf("exact path limit rejected: %v", err)
	}

	over := exact + "x"
	err := ValidateRelativePath(over)
	var pathErr *PathValidationError
	if !errors.As(err, &pathErr) || pathErr.Problem != PathProblemTooLong {
		t.Fatalf("overlong path error = %v, want PathProblemTooLong", err)
	}
}

func TestValidateUserRelativePathRejectsReservedRoots(t *testing.T) {
	t.Parallel()

	for _, relative := range []string{
		".remember/index.db",
		".REMEMBER/index.db",
		"_Konflikte/note.md",
		"_KONFLIKTE/Wiederhergestellt/note.md",
	} {
		err := ValidateUserRelativePath(relative)
		var pathErr *PathValidationError
		if !errors.As(err, &pathErr) || pathErr.Problem != PathProblemReserved {
			t.Errorf("ValidateUserRelativePath(%q) error = %v, want reserved", relative, err)
		}
	}

	for _, relative := range []string{
		"ordinary/.remember/note.md",
		"ordinary/_Konflikte/note.md",
		"Wiederhergestellt/note.md",
	} {
		if err := ValidateUserRelativePath(relative); err != nil {
			t.Errorf("ValidateUserRelativePath(%q) unexpected error: %v", relative, err)
		}
	}
}
