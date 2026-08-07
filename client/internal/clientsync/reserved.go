package clientsync

import (
	"path"
	"strings"
	"unicode/utf8"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

var (
	ConflictRootID      = uuid.MustParse("7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f001")
	ConflictRecoveredID = uuid.MustParse("7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002")
)

func ConflictFileName(original string, operationID uuid.UUID) string {
	stem := strings.TrimSuffix(original, path.Ext(original))
	suffix := " (Konflikt - " + operationID.String() + ").md"
	limit := naming.MaxComponentBytes - len(suffix)
	for len(stem) > limit {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	if stem == "" {
		stem = "Notiz"
	}
	return stem + suffix
}

func IsReservedConflictFolder(id uuid.UUID) bool {
	return id == ConflictRootID || id == ConflictRecoveredID
}

const (
	ConflictRootName      = "_Konflikte"
	ConflictRecoveredName = "Wiederhergestellt"
)
