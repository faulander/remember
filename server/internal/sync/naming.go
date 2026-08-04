package sync

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxNameBytes = 180
	maxPathBytes = 768
)

var fold = cases.Fold()

func normalizeName(input string) (string, string, error) {
	if !utf8.ValidString(input) {
		return "", "", fmt.Errorf("%w: name is not UTF-8", ErrInvalidInput)
	}
	name := norm.NFC.String(input)
	if name == "" || len(name) > maxNameBytes {
		return "", "", fmt.Errorf("%w: invalid name length", ErrInvalidInput)
	}
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return "", "", fmt.Errorf("%w: forbidden name character", ErrInvalidInput)
		}
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") || reservedDevice(name) {
		return "", "", fmt.Errorf("%w: non-portable name", ErrInvalidInput)
	}
	return name, norm.NFC.String(fold.String(name)), nil
}

func validateLogicalPath(relative string) error {
	if len(relative) == 0 || len(relative) > maxPathBytes {
		return fmt.Errorf("%w: portable path length", ErrInvalidInput)
	}
	root, _, _ := strings.Cut(relative, "/")
	rootKey := norm.NFC.String(fold.String(root))
	if rootKey == norm.NFC.String(fold.String(".remember")) ||
		rootKey == norm.NFC.String(fold.String("_Konflikte")) {
		return fmt.Errorf("%w: reserved root path", ErrInvalidInput)
	}
	return nil
}

func reservedDevice(name string) bool {
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
