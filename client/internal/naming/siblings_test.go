package naming

import (
	"errors"
	"reflect"
	"testing"
)

func TestFindSiblingCollisions(t *testing.T) {
	t.Parallel()

	names := []string{
		"Note.md", "note.MD",
		"Straße.md", "STRASSE.MD", "strasse.md",
		"Other.md",
	}
	got, err := FindSiblingCollisions(names)
	if err != nil {
		t.Fatalf("FindSiblingCollisions() error = %v", err)
	}

	want := []Collision{
		{Key: CollisionKey("Note.md"), Names: []string{"Note.md", "note.MD"}},
		{Key: CollisionKey("Straße.md"), Names: []string{"Straße.md", "STRASSE.MD", "strasse.md"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FindSiblingCollisions() = %#v, want %#v", got, want)
	}
}

func TestFindSiblingCollisionsRejectsInvalidWithoutMutation(t *testing.T) {
	t.Parallel()

	names := []string{"Valid.md", "Cafe\u0301.md"}
	before := append([]string(nil), names...)
	_, err := FindSiblingCollisions(names)
	var siblingErr *SiblingValidationError
	if !errors.As(err, &siblingErr) {
		t.Fatalf("error = %v, want SiblingValidationError", err)
	}
	if siblingErr.Index != 1 {
		t.Errorf("invalid index = %d, want 1", siblingErr.Index)
	}
	if !reflect.DeepEqual(names, before) {
		t.Errorf("input mutated: got %q, want %q", names, before)
	}
}

func TestEveryWindowsForbiddenCharacterIsRejected(t *testing.T) {
	t.Parallel()

	for _, r := range `<>:"/\|?*` {
		name := "before" + string(r) + "after.md"
		if err := ValidateComponent(name); err == nil {
			t.Errorf("ValidateComponent(%q) accepted forbidden character %q", name, r)
		}
	}
	for r := rune(0); r < 0x20; r++ {
		name := "before" + string(r) + "after.md"
		if err := ValidateComponent(name); err == nil {
			t.Errorf("ValidateComponent() accepted control character U+%04X", r)
		}
	}
}
