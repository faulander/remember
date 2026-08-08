package frontmatter

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	existingID  = "550e8400-e29b-41d4-a716-446655440000"
	candidateID = "018f4c3a-1234-7abc-8123-123456789abc"
)

func TestInspect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		markdown    string
		want        Inspection
		wantProblem Problem
	}{
		{name: "no frontmatter", markdown: "# Note\n", want: Inspection{}},
		{
			name:     "unrelated frontmatter",
			markdown: "---\ntitle: Example\n---\nBody\n",
			want:     Inspection{HasFrontmatter: true},
		},
		{
			name:     "valid v1",
			markdown: "---\nremember:\n  schema: 1\n  note_id: \"" + existingID + "\"\n---\nBody\n",
			want: Inspection{
				HasFrontmatter: true,
				HasRemember:    true,
				NoteID:         uuid.MustParse(existingID),
				Schema:         1,
			},
		},
		{name: "unclosed", markdown: "---\ntitle: no end\n", wantProblem: ProblemUnclosed},
		{name: "invalid yaml", markdown: "---\n[broken\n---\n", wantProblem: ProblemInvalidYAML},
		{name: "non mapping root", markdown: "---\n- item\n---\n", wantProblem: ProblemInvalidRoot},
		{name: "duplicate top key", markdown: "---\ntitle: one\ntitle: two\n---\n", wantProblem: ProblemDuplicateKey},
		{name: "duplicate remember key", markdown: "---\nremember:\n  schema: 1\n  schema: 1\n  note_id: \"" + existingID + "\"\n---\n", wantProblem: ProblemDuplicateKey},
		{name: "remember not mapping", markdown: "---\nremember: true\n---\n", wantProblem: ProblemInvalidRemember},
		{name: "missing schema", markdown: "---\nremember:\n  note_id: \"" + existingID + "\"\n---\n", wantProblem: ProblemMissingSchema},
		{name: "future schema", markdown: "---\nremember:\n  schema: 2\n  note_id: \"" + existingID + "\"\n---\n", wantProblem: ProblemUnsupportedSchema},
		{name: "string schema", markdown: "---\nremember:\n  schema: \"1\"\n  note_id: \"" + existingID + "\"\n---\n", wantProblem: ProblemUnsupportedSchema},
		{name: "missing id", markdown: "---\nremember:\n  schema: 1\n---\n", wantProblem: ProblemMissingNoteID},
		{name: "invalid id", markdown: "---\nremember:\n  schema: 1\n  note_id: nope\n---\n", wantProblem: ProblemInvalidNoteID},
		{name: "nil id", markdown: "---\nremember:\n  schema: 1\n  note_id: \"00000000-0000-0000-0000-000000000000\"\n---\n", wantProblem: ProblemInvalidNoteID},
		{name: "non RFC variant", markdown: "---\nremember:\n  schema: 1\n  note_id: \"550e8400-e29b-41d4-c716-446655440000\"\n---\n", wantProblem: ProblemInvalidNoteID},
		{name: "uppercase id", markdown: "---\nremember:\n  schema: 1\n  note_id: \"550E8400-E29B-41D4-A716-446655440000\"\n---\n", wantProblem: ProblemInvalidNoteID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := []byte(tt.markdown)
			before := append([]byte(nil), original...)
			got, err := Inspect(original)
			if tt.wantProblem != "" {
				var validationErr *ValidationError
				if !errors.As(err, &validationErr) || validationErr.Problem != tt.wantProblem {
					t.Fatalf("Inspect() error = %v, want problem %q", err, tt.wantProblem)
				}
			} else {
				if err != nil {
					t.Fatalf("Inspect() error = %v", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("Inspect() = %#v, want %#v", got, tt.want)
				}
			}
			if !bytes.Equal(original, before) {
				t.Error("Inspect() modified input bytes")
			}
		})
	}
}

func TestEnsureIdentityAddsFrontmatter(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse(candidateID)
	result, err := EnsureIdentity([]byte("# Note\n\nBody\n"), id)
	if err != nil {
		t.Fatalf("EnsureIdentity() error = %v", err)
	}
	if !result.Changed || result.NoteID != id {
		t.Fatalf("EnsureIdentity() result = %#v", result)
	}
	if !bytes.HasSuffix(result.Markdown, []byte("# Note\n\nBody\n")) {
		t.Errorf("Markdown body changed:\n%s", result.Markdown)
	}
	inspection, err := Inspect(result.Markdown)
	if err != nil {
		t.Fatalf("Inspect(patched) error = %v", err)
	}
	if inspection.NoteID != id || inspection.Schema != CurrentSchema {
		t.Errorf("patched inspection = %#v", inspection)
	}
}

func TestEnsureIdentityUsesExistingCRLFWithoutFrontmatter(t *testing.T) {
	t.Parallel()

	input := []byte("# Note\r\nBody\r\n")
	result, err := EnsureIdentity(input, uuid.MustParse(candidateID))
	if err != nil {
		t.Fatalf("EnsureIdentity() error = %v", err)
	}
	if strings.Contains(strings.ReplaceAll(string(result.Markdown), "\r\n", ""), "\n") {
		t.Error("patched CRLF document contains bare LF")
	}
	if !bytes.HasSuffix(result.Markdown, input) {
		t.Error("CRLF Markdown body changed")
	}
}

func TestEnsureIdentityAcceptsCanonicalRFC4122Candidate(t *testing.T) {
	t.Parallel()

	result, err := EnsureIdentity([]byte("Body\n"), uuid.MustParse(existingID))
	if err != nil || result.NoteID.String() != existingID {
		t.Fatalf("EnsureIdentity() result=%#v error=%v", result, err)
	}
}

func TestEnsureIdentityPreservesUnknownFieldsCommentsAndBody(t *testing.T) {
	t.Parallel()

	input := []byte("---\r\n# title comment\r\ntitle: Example\r\ncustom:\r\n  enabled: true\r\n---\r\n# Body\r\nline\r\n")
	id := uuid.MustParse(candidateID)
	result, err := EnsureIdentity(input, id)
	if err != nil {
		t.Fatalf("EnsureIdentity() error = %v", err)
	}
	text := string(result.Markdown)
	for _, preserved := range []string{"# title comment", "title: Example", "custom:", "enabled: true", "# Body\r\nline\r\n"} {
		if !strings.Contains(text, preserved) {
			t.Errorf("patched Markdown does not preserve %q:\n%s", preserved, text)
		}
	}
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Error("patched CRLF document contains bare LF")
	}
}

func TestEnsureIdentityLeavesExistingIdentityByteExact(t *testing.T) {
	t.Parallel()

	input := []byte("---\nremember: {schema: 1, note_id: \"" + existingID + "\"}\n---\nBody\n")
	candidate := uuid.MustParse("018f4c3a-1234-7abc-8123-123456789abc")
	result, err := EnsureIdentity(input, candidate)
	if err != nil {
		t.Fatalf("EnsureIdentity() error = %v", err)
	}
	if result.Changed {
		t.Error("existing identity was marked changed")
	}
	if result.NoteID.String() != existingID {
		t.Errorf("NoteID = %s, want existing %s", result.NoteID, existingID)
	}
	if !bytes.Equal(result.Markdown, input) {
		t.Error("existing valid document was reserialized")
	}
}

func TestMaterializeCanonicalAbsentCopiesAllowMissingRevision(t *testing.T) {
	original, conflictID, operationID := uuid.New(), uuid.New(), uuid.Must(uuid.NewV7())
	input := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + original.String() + "\"\n---\nbody\n")
	for _, reason := range []string{"path_collision", "object_missing", "parent_unavailable"} {
		if _, err := MaterializeConflictCopy(input, original, conflictID, ConflictOrigin{OriginalNoteID: original, OperationID: operationID, Reason: reason, OriginalTarget: "N.md"}); err != nil {
			t.Fatalf("reason %s: %v", reason, err)
		}
	}
}

func TestMaterializeConflictCopyRekeysAndRecordsOrigin(t *testing.T) {
	original, conflictID, operationID := uuid.New(), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	input := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + original.String() + "\"\n  custom: keep\n---\nbody\n")
	output, err := MaterializeConflictCopy(input, original, conflictID, ConflictOrigin{OriginalNoteID: original, OperationID: operationID, Reason: "base_revision_mismatch", OriginalTarget: "N.md", BaseRevision: 1, CanonicalRevision: 2})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := Inspect(output)
	if err != nil || inspection.NoteID != conflictID {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	for _, expected := range []string{"custom: keep", "original_note_id: \"" + original.String() + "\"", "operation_id: \"" + operationID.String() + "\"", "reason: base_revision_mismatch", "body"} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Fatalf("missing %q in %s", expected, output)
		}
	}
}

func TestEnsureIdentityRefusesInvalidSource(t *testing.T) {
	t.Parallel()

	input := []byte("---\nremember: broken\n---\nBody\n")
	before := append([]byte(nil), input...)
	_, err := EnsureIdentity(input, uuid.MustParse(candidateID))
	if err == nil {
		t.Fatal("EnsureIdentity() accepted invalid Remember data")
	}
	if !bytes.Equal(input, before) {
		t.Error("EnsureIdentity() modified invalid input")
	}
}

func TestReadAndUpdateEditableTagsPreserveIdentityAndUnknownFields(t *testing.T) {
	t.Parallel()

	input := []byte("---\n# keep\ntitle: Unknown\nremember:\n  schema: 1\n  note_id: \"" + existingID + "\"\n  tags: [Work, später]\n  custom: true\n---\nOld body\n")
	document, err := Read(input)
	if err != nil {
		t.Fatal(err)
	}
	if document.NoteID.String() != existingID || string(document.Body) != "Old body\n" || !reflect.DeepEqual(document.Tags, []string{"Work", "später"}) {
		t.Fatalf("Read() = %#v", document)
	}
	updated, err := UpdateEditable(input, document.NoteID, []byte("New body\n"), []string{" work ", "Café", "cafe\u0301"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, expected := range []string{"# keep", "title: Unknown", "custom: true", "New body"} {
		if !strings.Contains(text, expected) {
			t.Errorf("updated document lost %q:\n%s", expected, text)
		}
	}
	inspection, err := Inspect(updated)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.NoteID != document.NoteID || !reflect.DeepEqual(inspection.Tags, []string{"work", "Café"}) {
		t.Errorf("updated inspection = %#v", inspection)
	}
}

func TestTagsRejectInvalidStoredAndInputValues(t *testing.T) {
	t.Parallel()

	base := "---\nremember:\n  schema: 1\n  note_id: \"" + existingID + "\"\n  tags: %s\n---\nBody\n"
	for _, yamlTags := range []string{"work", "[Work, work]", "[\" bad\"]", "[\"line\\nfeed\"]"} {
		_, err := Inspect([]byte(fmt.Sprintf(base, yamlTags)))
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) || validationErr.Problem != ProblemInvalidTags {
			t.Errorf("stored tags %q error = %v", yamlTags, err)
		}
	}
	input := []byte(fmt.Sprintf(base, "[]"))
	for _, tags := range [][]string{{""}, {strings.Repeat("x", 41)}, {strings.Repeat("界", 27)}, {"bad\u0001tag"}} {
		if _, err := UpdateEditable(input, uuid.MustParse(existingID), []byte("Body"), tags); err == nil {
			t.Errorf("invalid input tags accepted: %#v", tags)
		}
	}
}

func TestUpdateEditableRejectsIdentityMismatch(t *testing.T) {
	t.Parallel()
	input := []byte("---\nremember:\n  schema: 1\n  note_id: \"" + existingID + "\"\n---\nBody\n")
	if _, err := UpdateEditable(input, uuid.MustParse(candidateID), []byte("changed"), nil); err == nil {
		t.Fatal("identity mismatch accepted")
	}
}

func TestNewNoteIDUsesUUIDv7(t *testing.T) {
	t.Parallel()

	id, err := NewNoteID()
	if err != nil {
		t.Fatalf("NewNoteID() error = %v", err)
	}
	if id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		t.Errorf("NewNoteID() = %s, want RFC UUIDv7", id)
	}
}

func FuzzInspectAndPatch(f *testing.F) {
	for _, seed := range []string{
		"Body\n",
		"---\ntitle: Example\n---\nBody\n",
		"---\nremember:\n  schema: 1\n  note_id: \"" + existingID + "\"\n---\n",
		"---\n[broken\n---\n",
	} {
		f.Add([]byte(seed))
	}
	candidate := uuid.MustParse(candidateID)
	f.Fuzz(func(t *testing.T, input []byte) {
		before := bytes.Clone(input)
		_, _ = Inspect(input)
		if !bytes.Equal(input, before) {
			t.Fatal("Inspect() mutated fuzz input")
		}
		result, err := EnsureIdentity(input, candidate)
		if err == nil {
			inspection, inspectErr := Inspect(result.Markdown)
			if inspectErr != nil || inspection.NoteID == uuid.Nil {
				t.Fatalf("successful patch is not inspectable: %#v, %v", inspection, inspectErr)
			}
		}
	})
}
