package naming

import (
	"errors"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestValidateComponent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		problem Problem
	}{
		{name: "", problem: ProblemEmpty},
		{name: string([]byte{0xff}), problem: ProblemInvalidUTF8},
		{name: "Cafe\u0301.md", problem: ProblemNotNFC},
		{name: "bad/name.md", problem: ProblemForbiddenChar},
		{name: "bad\\name.md", problem: ProblemForbiddenChar},
		{name: "bad:name.md", problem: ProblemForbiddenChar},
		{name: "bad\x00name.md", problem: ProblemForbiddenChar},
		{name: "trailing ", problem: ProblemTrailingChar},
		{name: "trailing.", problem: ProblemTrailingChar},
		{name: "CON", problem: ProblemReservedDevice},
		{name: "con.md", problem: ProblemReservedDevice},
		{name: "CONIN$", problem: ProblemReservedDevice},
		{name: "conin$.txt", problem: ProblemReservedDevice},
		{name: "ConOut$", problem: ProblemReservedDevice},
		{name: "CONOUT$.log", problem: ProblemReservedDevice},
		{name: "PRN.backup.md", problem: ProblemReservedDevice},
		{name: "COM1", problem: ProblemReservedDevice},
		{name: "com9.txt", problem: ProblemReservedDevice},
		{name: "LPT1", problem: ProblemReservedDevice},
		{name: "lpt9.md", problem: ProblemReservedDevice},
		{name: "COM¹.txt", problem: ProblemReservedDevice},
		{name: "LPT³", problem: ProblemReservedDevice},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateComponent(tt.name)
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateComponent(%q) error = %v, want ValidationError", tt.name, err)
			}
			if validationErr.Problem != tt.problem {
				t.Errorf("ValidateComponent(%q) problem = %q, want %q", tt.name, validationErr.Problem, tt.problem)
			}
		})
	}
}

func TestValidateComponentAcceptsPortableNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"Note.md",
		"Café.md",
		"日本語のノート.md",
		"COM0.md",
		"COM10.md",
		"LPT0",
		"auxiliary.md",
		"name with spaces.md",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateComponent(name); err != nil {
				t.Errorf("ValidateComponent(%q) returned unexpected error: %v", name, err)
			}
		})
	}
}

func TestNormalizeAndValidateComponent(t *testing.T) {
	t.Parallel()

	got, err := NormalizeAndValidateComponent("Cafe\u0301.md")
	if err != nil {
		t.Fatalf("NormalizeAndValidateComponent() error = %v", err)
	}
	if want := "Café.md"; got != want {
		t.Errorf("NormalizeAndValidateComponent() = %q, want %q", got, want)
	}

	if err := ValidateComponent("Cafe\u0301.md"); err == nil {
		t.Error("ValidateComponent() accepted a non-NFC external name")
	}
}

func TestCollisionKey(t *testing.T) {
	t.Parallel()

	equivalent := [][]string{
		{"Note.md", "note.MD"},
		{"Straße.md", "STRASSE.MD"},
		{"Café.md", "CAFE\u0301.MD"},
	}

	for _, names := range equivalent {
		first := CollisionKey(names[0])
		for _, name := range names[1:] {
			if got := CollisionKey(name); got != first {
				t.Errorf("CollisionKey(%q) = %q, want %q from %q", name, got, first, names[0])
			}
		}
	}

	if CollisionKey("note.md") == CollisionKey("notes.md") {
		t.Error("different names produced the same collision key")
	}
}

func FuzzNormalizeAndValidateComponent(f *testing.F) {
	for _, seed := range []string{"Note.md", "Cafe\u0301.md", "日本語.md", "CON", "bad/name"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		normalized, err := NormalizeAndValidateComponent(name)
		if err == nil && !norm.NFC.IsNormalString(normalized) {
			t.Fatalf("valid normalized name %q is not NFC", normalized)
		}
	})
}
