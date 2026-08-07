# Remember-Dokumentation

## Produkt und Architektur

- [`PRD.md`](PRD.md) – verbindliche Produktanforderungen und Abnahmekriterien
- [`DESIGN.md`](DESIGN.md) – technisches Gesamtdesign
- [`MANUAL_TESTS_M1.md`](MANUAL_TESTS_M1.md) – manuelle Tests des lokalen Datenkerns
- [`MANUAL_TESTS_MAC_CLIENT.md`](MANUAL_TESTS_MAC_CLIENT.md) – abgenommene macOS-Clienttests

## Architecture Decision Records

| ADR | Entscheidung |
|---|---|
| [0001](adr/0001-m1-local-format.md) | Lokales Format und Datenkern |
| [0002](adr/0002-client-ui-stack.md) | Wails, Svelte 5, TypeScript und Vite |
| [0003](adr/0003-m2-identity-core.md) | Interner Identity-Core |
| [0004](adr/0004-local-note-editor.md) | Lokaler Notizeditor |
| [0005](adr/0005-m2-sync-core.md) | Mandantengebundener Sync-Core |
| [0006](adr/0006-m2-blob-repository.md) | Internes Blob-Repository |
| [0007](adr/0007-m2-sessions-devices-core.md) | Sessions- und Devices-Core |
| [0008](adr/0008-m2-auth-http-transport.md) | Begrenzter Auth-HTTP-Transport |
| [0009](adr/0009-m2-blob-http-transport.md) | Authentifizierter Blob-HTTP-Transport und Benutzerquota |
| [0010](adr/0010-m2-sync-http-transport.md) | Authentifizierter Sync-HTTP-Transport |
| [0011](adr/0011-m2-client-sync-durability.md) | Client-Outbox, Blob-Staging und Apply-Plan-Persistenz |
| [0012](adr/0012-m2-client-foreground-sync.md) | Begrenzter Client-HTTP-Transport und Vordergrund-Sync |
| [0013](adr/0013-m2-folder-create-apply.md) | Identitätsgebundener Folder-Create-Apply |
| [0014](adr/0014-m2-folder-move-delete-apply.md) | Inode-gebundener Folder-Move/-Delete-Apply |
| [0015](adr/0015-m2-conflict-canonical-state.md) | Authentifizierter kanonischer Zustand bei Sync-Konflikten |
| [0016](adr/0016-m2-reserved-conflict-namespace.md) | Server-provisionierter reservierter Konfliktbereich |

ADRs dokumentieren einen zum jeweiligen Zeitpunkt akzeptierten Schnitt. Spätere Entscheidungen dürfen frühere ADRs ergänzen, müssen Abweichungen aber ausdrücklich benennen.
