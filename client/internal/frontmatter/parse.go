// Package frontmatter inspects and patches Remember's versioned YAML metadata.
package frontmatter

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const CurrentSchema = 1

// Problem identifies a frontmatter problem suitable for a local issue code.
type Problem string

const (
	ProblemUnclosed          Problem = "unclosed_frontmatter"
	ProblemInvalidYAML       Problem = "invalid_yaml"
	ProblemDuplicateKey      Problem = "duplicate_yaml_key"
	ProblemInvalidRoot       Problem = "invalid_yaml_root"
	ProblemInvalidRemember   Problem = "invalid_remember_mapping"
	ProblemMissingSchema     Problem = "missing_schema"
	ProblemUnsupportedSchema Problem = "unsupported_schema"
	ProblemMissingNoteID     Problem = "missing_note_id"
	ProblemInvalidNoteID     Problem = "invalid_note_id"
	ProblemInvalidTags       Problem = "invalid_tags"
)

// ValidationError is returned without modifying the source Markdown.
type ValidationError struct {
	Problem Problem
	Detail  string
}

func (e *ValidationError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("invalid frontmatter: %s", e.Problem)
	}
	return fmt.Sprintf("invalid frontmatter: %s: %s", e.Problem, e.Detail)
}

// Inspection is the read-only result of parsing a Markdown document.
type Inspection struct {
	HasFrontmatter bool
	HasRemember    bool
	NoteID         uuid.UUID
	Schema         int
	Tags           []string
}

// Document is the editable projection. Identity metadata remains hidden.
type Document struct {
	NoteID uuid.UUID
	Body   []byte
	Tags   []string
}

type parsedDocument struct {
	inspection Inspection
	document   *yaml.Node
	root       *yaml.Node
	remember   *yaml.Node
	body       []byte
	newline    string
}

// Inspect validates Remember metadata and never changes input bytes.
func Inspect(markdown []byte) (Inspection, error) {
	parsed, err := parse(markdown)
	if err != nil {
		return Inspection{}, err
	}
	return parsed.inspection, nil
}

// Read returns only the editable body and tags plus the stable identity.
func Read(markdown []byte) (Document, error) {
	parsed, err := parse(markdown)
	if err != nil {
		return Document{}, err
	}
	return Document{
		NoteID: parsed.inspection.NoteID,
		Body:   append([]byte(nil), parsed.body...),
		Tags:   append([]string(nil), parsed.inspection.Tags...),
	}, nil
}

func parse(markdown []byte) (*parsedDocument, error) {
	yamlBytes, body, newline, hasFrontmatter, err := split(markdown)
	if err != nil {
		return nil, err
	}
	if !hasFrontmatter {
		newline := "\n"
		if bytes.Contains(markdown, []byte("\r\n")) {
			newline = "\r\n"
		}
		return &parsedDocument{
			inspection: Inspection{},
			body:       markdown,
			newline:    newline,
		}, nil
	}

	document := &yaml.Node{}
	if len(bytes.TrimSpace(yamlBytes)) == 0 {
		document.Kind = yaml.DocumentNode
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	} else if err := yaml.Unmarshal(yamlBytes, document); err != nil {
		return nil, &ValidationError{Problem: ProblemInvalidYAML, Detail: err.Error()}
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, &ValidationError{Problem: ProblemInvalidRoot}
	}
	root := document.Content[0]
	if duplicate := findDuplicateKey(root); duplicate != "" {
		return nil, &ValidationError{Problem: ProblemDuplicateKey, Detail: duplicate}
	}

	result := &parsedDocument{
		inspection: Inspection{HasFrontmatter: true},
		document:   document,
		root:       root,
		body:       body,
		newline:    newline,
	}
	remember := mappingValue(root, "remember")
	if remember == nil {
		return result, nil
	}
	result.inspection.HasRemember = true
	result.remember = remember
	if remember.Kind != yaml.MappingNode {
		return nil, &ValidationError{Problem: ProblemInvalidRemember}
	}

	schemaNode := mappingValue(remember, "schema")
	if schemaNode == nil {
		return nil, &ValidationError{Problem: ProblemMissingSchema}
	}
	if schemaNode.Kind != yaml.ScalarNode || schemaNode.Tag != "!!int" {
		return nil, &ValidationError{Problem: ProblemUnsupportedSchema, Detail: schemaNode.Value}
	}
	schema, err := strconv.Atoi(schemaNode.Value)
	if err != nil || schema != CurrentSchema {
		return nil, &ValidationError{Problem: ProblemUnsupportedSchema, Detail: schemaNode.Value}
	}
	result.inspection.Schema = schema

	idNode := mappingValue(remember, "note_id")
	if idNode == nil {
		return nil, &ValidationError{Problem: ProblemMissingNoteID}
	}
	if idNode.Kind != yaml.ScalarNode || idNode.Tag != "!!str" {
		return nil, &ValidationError{Problem: ProblemInvalidNoteID, Detail: idNode.Value}
	}
	id, err := uuid.Parse(idNode.Value)
	if err != nil || id == uuid.Nil || id.Variant() != uuid.RFC4122 || id.String() != idNode.Value {
		return nil, &ValidationError{Problem: ProblemInvalidNoteID, Detail: idNode.Value}
	}
	result.inspection.NoteID = id

	tagsNode := mappingValue(remember, "tags")
	if tagsNode != nil {
		tags, err := validateStoredTags(tagsNode)
		if err != nil {
			return nil, err
		}
		result.inspection.Tags = tags
	}
	return result, nil
}

var tagFold = cases.Fold()

// NormalizeTags trims UI input, normalizes it to NFC and enforces tag policy.
func NormalizeTags(tags []string) ([]string, error) {
	result := make([]string, 0, len(tags))
	seen := make(map[string]struct{}, len(tags))
	for _, input := range tags {
		tag := norm.NFC.String(strings.TrimSpace(input))
		if err := validateTag(tag); err != nil {
			return nil, err
		}
		key := norm.NFC.String(tagFold.String(tag))
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, tag)
	}
	return result, nil
}

func validateStoredTags(node *yaml.Node) ([]string, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, &ValidationError{Problem: ProblemInvalidTags, Detail: "tags must be a sequence"}
	}
	result := make([]string, 0, len(node.Content))
	seen := make(map[string]struct{}, len(node.Content))
	for _, item := range node.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || !norm.NFC.IsNormalString(item.Value) {
			return nil, &ValidationError{Problem: ProblemInvalidTags, Detail: "tags must be NFC strings"}
		}
		if err := validateTag(item.Value); err != nil {
			return nil, err
		}
		key := norm.NFC.String(tagFold.String(item.Value))
		if _, exists := seen[key]; exists {
			return nil, &ValidationError{Problem: ProblemInvalidTags, Detail: "duplicate tag"}
		}
		seen[key] = struct{}{}
		result = append(result, item.Value)
	}
	return result, nil
}

func validateTag(tag string) error {
	if !utf8.ValidString(tag) || tag == "" || utf8.RuneCountInString(tag) > 40 || len(tag) > 80 ||
		tag != strings.TrimSpace(tag) {
		return &ValidationError{Problem: ProblemInvalidTags, Detail: "tag length or whitespace is invalid"}
	}
	for _, r := range tag {
		if unicode.IsControl(r) {
			return &ValidationError{Problem: ProblemInvalidTags, Detail: "tag contains a control character"}
		}
	}
	return nil
}

func split(markdown []byte) (yamlBytes, body []byte, newline string, has bool, err error) {
	if !bytes.HasPrefix(markdown, []byte("---\n")) && !bytes.HasPrefix(markdown, []byte("---\r\n")) {
		return nil, markdown, "\n", false, nil
	}

	openingEnd := bytes.IndexByte(markdown, '\n') + 1
	newline = "\n"
	if openingEnd >= 2 && markdown[openingEnd-2] == '\r' {
		newline = "\r\n"
	}

	for start := openingEnd; start <= len(markdown); {
		relativeEnd := bytes.IndexByte(markdown[start:], '\n')
		lineEnd := len(markdown)
		next := len(markdown)
		if relativeEnd >= 0 {
			lineEnd = start + relativeEnd
			next = lineEnd + 1
		}
		line := markdown[start:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if bytes.Equal(line, []byte("---")) {
			return markdown[openingEnd:start], markdown[next:], newline, true, nil
		}
		if relativeEnd < 0 {
			break
		}
		start = next
	}

	return nil, nil, newline, true, &ValidationError{Problem: ProblemUnclosed}
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind == yaml.ScalarNode && mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func findDuplicateKey(node *yaml.Node) string {
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode {
				identity := key.Tag + "\x00" + key.Value
				if _, exists := seen[identity]; exists {
					return key.Value
				}
				seen[identity] = struct{}{}
			}
		}
	}
	for _, child := range node.Content {
		if duplicate := findDuplicateKey(child); duplicate != "" {
			return duplicate
		}
	}
	return ""
}
