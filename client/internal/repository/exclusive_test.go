package repository

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateExclusiveAndMoveExpectedNeverClobber(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.md")
	destination := filepath.Join(directory, "destination.md")
	if err := CreateExclusive(source, []byte("source"), nil); err != nil {
		t.Fatal(err)
	}
	if err := CreateExclusive(source, []byte("replacement"), nil); err == nil {
		t.Fatal("exclusive create clobbered source")
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MoveExclusiveExpected(source, destination, []byte("source")); err == nil {
		t.Fatal("exclusive move clobbered destination")
	}
	content, _ := os.ReadFile(destination)
	if string(content) != "destination" {
		t.Errorf("destination = %q", content)
	}
}

func TestMoveExpectedKeepsSourceOnRevisionMismatch(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.md")
	destination := filepath.Join(directory, "destination.md")
	if err := os.WriteFile(source, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := MoveExclusiveExpected(source, destination, []byte("expected"))
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("MoveExclusiveExpected() error = %v", err)
	}
	if content, err := os.ReadFile(source); err != nil || string(content) != "external" {
		t.Errorf("source missing or changed: %q, %v", content, err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Errorf("temporary destination remains: %v", err)
	}
}
