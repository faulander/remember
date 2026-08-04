// Package identity implements the internal account bootstrap core.
package identity

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

var ErrInvalidEmail = errors.New("invalid email address")

// Email contains the delivery form and the uniqueness key.
type Email struct {
	Delivery  string
	Canonical string
}

// CanonicalizeEmail applies Identity Policy v1 without provider-specific rules.
func CanonicalizeEmail(input string) (Email, error) {
	if !utf8.ValidString(input) {
		return Email{}, ErrInvalidEmail
	}
	trimmed := strings.Trim(input, " \t")
	if trimmed == "" || strings.Count(trimmed, "@") != 1 {
		return Email{}, ErrInvalidEmail
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return Email{}, ErrInvalidEmail
		}
	}
	local, domain, _ := strings.Cut(trimmed, "@")
	if len(local) == 0 || len(local) > 64 || !validDotAtom(local) {
		return Email{}, ErrInvalidEmail
	}
	if domain == "" || strings.HasPrefix(domain, "[") || strings.HasSuffix(domain, ".") {
		return Email{}, ErrInvalidEmail
	}
	asciiDomain, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return Email{}, ErrInvalidEmail
	}
	asciiDomain = strings.ToLower(asciiDomain)
	if len(asciiDomain) == 0 || len(asciiDomain) > 253 {
		return Email{}, ErrInvalidEmail
	}
	for _, label := range strings.Split(asciiDomain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return Email{}, ErrInvalidEmail
		}
	}
	delivery := local + "@" + asciiDomain
	canonical := strings.ToLower(local) + "@" + asciiDomain
	if len(delivery) > 254 || len(canonical) > 254 {
		return Email{}, ErrInvalidEmail
	}
	return Email{Delivery: delivery, Canonical: canonical}, nil
}

func validDotAtom(local string) bool {
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	for i := 0; i < len(local); i++ {
		c := local[i]
		if c >= 0x80 || !isAText(c) {
			return false
		}
	}
	return true
}

func isAText(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-/=?^_`{|}~.", rune(c))
}
