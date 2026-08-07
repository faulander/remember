package clientsync

import "github.com/google/uuid"

var (
	ConflictRootID      = uuid.MustParse("7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f001")
	ConflictRecoveredID = uuid.MustParse("7a3e2b0e-6a3d-4c5f-8a61-9a0c8d47f002")
)

const (
	ConflictRootName      = "_Konflikte"
	ConflictRecoveredName = "Wiederhergestellt"
)
