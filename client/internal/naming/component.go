// Package naming defines platform-independent logical name rules.
package naming

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Problem identifies why a logical name is not portable.
type Problem string

const (
	// PolicyVersion changes when persisted logical-name semantics change.
	PolicyVersion = 1

	// MaxComponentBytes is measured after NFC normalization.
	MaxComponentBytes = 180
	// MaxRelativePathBytes includes logical '/' separators.
	MaxRelativePathBytes = 768
	// ConflictSuffixReserveBytes is retained when deriving conflict names.
	ConflictSuffixReserveBytes = 64

	ProblemEmpty          Problem = "empty"
	ProblemInvalidUTF8    Problem = "invalid_utf8"
	ProblemNotNFC         Problem = "not_nfc"
	ProblemForbiddenChar  Problem = "forbidden_character"
	ProblemTrailingChar   Problem = "trailing_character"
	ProblemReservedDevice Problem = "reserved_device_name"
	ProblemTooLong        Problem = "too_long"
)

// ValidationError reports a portable-name validation problem.
type ValidationError struct {
	Problem Problem
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid portable name: %s", e.Problem)
}

var fold = cases.Fold()

// ValidateComponent validates one path component without changing it.
// External filesystem names should use this function so invalid names are
// reported rather than silently normalized.
func ValidateComponent(name string) error {
	if name == "" {
		return &ValidationError{Problem: ProblemEmpty}
	}
	if !utf8.ValidString(name) {
		return &ValidationError{Problem: ProblemInvalidUTF8}
	}
	if !norm.NFC.IsNormalString(name) {
		return &ValidationError{Problem: ProblemNotNFC}
	}
	if len(name) > MaxComponentBytes {
		return &ValidationError{Problem: ProblemTooLong}
	}

	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return &ValidationError{Problem: ProblemForbiddenChar}
		}
	}

	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return &ValidationError{Problem: ProblemTrailingChar}
	}
	if isReservedWindowsDevice(name) {
		return &ValidationError{Problem: ProblemReservedDevice}
	}

	return nil
}

// NormalizeAndValidateComponent normalizes app-entered text to NFC and then
// validates it. It must not be used to rewrite externally discovered names.
func NormalizeAndValidateComponent(name string) (string, error) {
	if !utf8.ValidString(name) {
		return name, &ValidationError{Problem: ProblemInvalidUTF8}
	}

	normalized := norm.NFC.String(name)
	return normalized, ValidateComponent(normalized)
}

// CollisionKey returns the NFC-normalized Unicode case-folded key used to
// compare sibling names across platforms.
func CollisionKey(name string) string {
	return norm.NFC.String(fold.String(norm.NFC.String(name)))
}

func isReservedWindowsDevice(name string) bool {
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))

	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}

	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		return stem[3] >= '1' && stem[3] <= '9'
	}

	return stem == "COM¹" || stem == "COM²" || stem == "COM³" ||
		stem == "LPT¹" || stem == "LPT²" || stem == "LPT³"
}
