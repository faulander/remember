package frontmatter

import (
	"bytes"
	"strings"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
)

// PatchResult contains the complete Markdown document after identity handling.
type PatchResult struct {
	Markdown []byte
	NoteID   uuid.UUID
	Changed  bool
}

// NewNoteID creates a time-ordered UUIDv7 note identity.
func NewNoteID() (uuid.UUID, error) {
	return uuid.NewV7()
}

// EnsureIdentity adds a Remember v1 identity only when none exists. Existing
// valid identities are never replaced or reserialized. The caller supplies the
// candidate ID so retries and tests can remain deterministic.
func EnsureIdentity(markdown []byte, candidate uuid.UUID) (PatchResult, error) {
	parsed, err := parse(markdown)
	if err != nil {
		return PatchResult{}, err
	}
	if parsed.inspection.HasRemember {
		return PatchResult{
			Markdown: markdown,
			NoteID:   parsed.inspection.NoteID,
			Changed:  false,
		}, nil
	}
	if candidate == uuid.Nil || candidate.Variant() != uuid.RFC4122 || candidate.Version() != 7 {
		return PatchResult{}, &ValidationError{Problem: ProblemInvalidNoteID, Detail: candidate.String()}
	}

	if parsed.document == nil {
		parsed.root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		parsed.document = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{parsed.root}}
	}
	remember := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	appendScalarPair(remember, "schema", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"})
	appendScalarPair(remember, "note_id", &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: candidate.String(),
		Style: yaml.DoubleQuotedStyle,
	})
	appendScalarPair(parsed.root, "remember", remember)

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(parsed.document); err != nil {
		return PatchResult{}, &ValidationError{Problem: ProblemInvalidYAML, Detail: err.Error()}
	}
	if err := encoder.Close(); err != nil {
		return PatchResult{}, &ValidationError{Problem: ProblemInvalidYAML, Detail: err.Error()}
	}
	yamlOutput := encoded.String()
	if parsed.newline == "\r\n" {
		yamlOutput = strings.ReplaceAll(yamlOutput, "\n", "\r\n")
	}

	output := make([]byte, 0, len(yamlOutput)+len(parsed.body)+16)
	output = append(output, "---"...)
	output = append(output, parsed.newline...)
	output = append(output, yamlOutput...)
	output = append(output, "---"...)
	output = append(output, parsed.newline...)
	output = append(output, parsed.body...)

	return PatchResult{Markdown: output, NoteID: candidate, Changed: true}, nil
}

// UpdateEditable replaces only body and tags while preserving stable identity
// and unknown YAML nodes. yaml.v3 may normalize frontmatter formatting.
func UpdateEditable(markdown []byte, expectedID uuid.UUID, body []byte, tags []string) ([]byte, error) {
	parsed, err := parse(markdown)
	if err != nil {
		return nil, err
	}
	if !parsed.inspection.HasRemember || expectedID == uuid.Nil || parsed.inspection.NoteID != expectedID {
		return nil, &ValidationError{Problem: ProblemInvalidNoteID, Detail: expectedID.String()}
	}
	normalizedTags, err := NormalizeTags(tags)
	if err != nil {
		return nil, err
	}
	if len(normalizedTags) == 0 {
		removeMappingPair(parsed.remember, "tags")
	} else {
		sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, tag := range normalizedTags {
			sequence.Content = append(sequence.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tag})
		}
		setMappingPair(parsed.remember, "tags", sequence)
	}

	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	encoder.SetIndent(2)
	if err := encoder.Encode(parsed.document); err != nil {
		return nil, &ValidationError{Problem: ProblemInvalidYAML, Detail: err.Error()}
	}
	if err := encoder.Close(); err != nil {
		return nil, &ValidationError{Problem: ProblemInvalidYAML, Detail: err.Error()}
	}
	yamlOutput := encoded.String()
	bodyOutput := string(body)
	if parsed.newline == "\r\n" {
		yamlOutput = strings.ReplaceAll(yamlOutput, "\n", "\r\n")
		bodyOutput = strings.ReplaceAll(strings.ReplaceAll(bodyOutput, "\r\n", "\n"), "\n", "\r\n")
	}
	output := make([]byte, 0, len(yamlOutput)+len(bodyOutput)+16)
	output = append(output, "---"...)
	output = append(output, parsed.newline...)
	output = append(output, yamlOutput...)
	output = append(output, "---"...)
	output = append(output, parsed.newline...)
	output = append(output, bodyOutput...)
	return output, nil
}

func setMappingPair(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind == yaml.ScalarNode && mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	appendScalarPair(mapping, key, value)
}

func removeMappingPair(mapping *yaml.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind == yaml.ScalarNode && mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

func appendScalarPair(mapping *yaml.Node, key string, value *yaml.Node) {
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}
