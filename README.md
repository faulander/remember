# Remember

Remember ist eine plattformübergreifende Local-first-Anwendung für persönliche Markdown-Notizen, Ordner und Erinnerungen. Normale `.md`-Dateien bleiben die kanonische lokale Datenquelle; ein zentraler Server synchronisiert mehrere eigene Geräte.

> **Status:** frühe Entwicklung, noch kein produktionsreifes Release. Builds sind derzeit unsigniert und werden manuell aktualisiert. Der aktuelle M2-Abdeckungsstand ist in der [expliziten Konfliktmatrix](docs/M2_CONFLICT_MATRIX.md) abgegrenzt; M2 ist noch nicht vollständig abgenommen.

## Aktueller Stand

- lokaler macOS-Desktop-Client mit Wails, Go, Svelte 5 und TypeScript
- echte Markdown-Dateien mit versioniertem YAML-Frontmatter
- verschachtelte Ordner, Tags, Vorschau, Themes und recoverbares Löschen
- lokaler SQLite-Index, Reconcile, Watcher sowie sichtbare Update/Update-, bidirektionale Edit/Delete-, bidirektionale Note-Move/Delete-, divergente Root-/Same-Parent-Nicht-Root- sowie äquivalente Root-/Nicht-Root-Note-Move-, fehlende Remote-Objekt-, fehlende Parent- und Note-Create/-Move-Pfadkonfliktkopien; leere Folder-Creates sowie streng manifestierte Folder mit ausschließlich direkten neuen Notes mit linearer, nie versuchter Edit-Historie werden bei Pfadkollision oder gelöschtem Parent sichtbar gerettet, leere oder streng manifestierte direkte-Note-Folder-Moves gegen Remote-Deletes sichtbar gerettet, Folder-Move-Pfad-/Parent-/Cycle-Konflikte inode-gebunden zurückgesetzt und äquivalente konkurrierende Moves ohne Pfadraten aufgelöst, bereits remote fehlende oder tombstoned Deletes konvergieren ohne neue Kopie, nichtleere Remote-Ordner werden sicher bewahrt und beschädigte Note-/Folder-Typzuordnungen und wiederverwendete Create-UUIDs bleiben ohne lokale Mutation fail-closed
- modularer Go-Server mit SQLite/WAL und sicherem Blob-Repository
- interner Identity-, Sync-, Sessions- und Devices-Core mit server-provisioniertem Konfliktbereich
- begrenzter HTTP-Transport für Authentifizierung, Sitzungs-/Geräteverwaltung, Blob-Bytes und idempotenten Cursor-Sync
- descriptor-rekursive exakte Subtree-Verifikation auf Darwin/Linux als Sicherheitsgrundlage für spätere Nested-Folder-Recovery; Windows bleibt dafür fail-closed
- lokaler Index v30 mit crash-sicherer Outbox, exakter Blob-Staging-Ablage, Konflikt-/Rebase-/No-op-/Folder-Restore-/Folder-Move-Revert-/Folder-Intent-/Blob-Cleanup-Journalen, descriptor-gebundener Löschung technischer Bytes und resumierbarem Notiz-/Folder-Apply
- strikter Client-HTTP-Transport und manueller Vordergrund-Sync für Notiz-CRUD sowie identitätsgebundene Folder-Create/-Move/-Delete-Operationen
- automatisierter Mehrgeräte-Konvergenztest über echte Login-, Blob- und Sync-HTTP-Routen einschließlich leerer divergenter Folder-Move-Recovery mit A/B/Restart/Cold-C, Serverneustart, kaltem History-Bootstrap, dauerhafter Pull-Seiten-Wiederaufnahme, sichtbarer Update-Konfliktkopie, bidirektionalen Note-Move/Delete-Konflikten, Direct-Note-Folder-Recovery mit linearen Updates, leerem beziehungsweise direct-note-haltigem Folder-Move gegen Remote-Delete, äquivalenten Root-/Nested-Note-Moves, divergentem Same-Parent-Note-Move sowie Note-/Folder-Move/-Delete
- lokale Schema-v30-Sync-Inbox mit atomarem Seiten-Ingest, exaktem Crash-Replay und einem begrenzten Scheduler, der bei ungelöstem Outbox-Konflikt unabhängige lineare Root-Note-Update/-Delete-Ketten über unveränderlich gebundene Einzelpläne anwendet, ohne den bestätigten Präfix zu überspringen; dies ist über echte Auth-/Blob-/Sync-Routen mit A/B und kaltem C hinter einem ungelösten divergenten Folder-Move abgenommen. Fehlende und hashinkonsistente Remote-Blobs werden dauerhaft als sichtbare Issues alarmiert und nach Wiederherstellung fortgesetzt; für divergente Folder-Moves ist kanonischer Serverpfad plus verlustfreie Recovery des lokalen Verlierers festgelegt; Strukturänderungen und vollständige Objektisolation bleiben für `SYNC-012` offen

Öffentliche Registrierung, Reminder, weitere Folder-/Strukturkonflikte und sichere Client-Tokenablage folgen in späteren Schnitten.

## Repository

```text
client/        Wails-Desktop-Client und Svelte-Frontend
server/        modularer Go-Server und Dockerfile
docs/          PRD, Design, ADRs und manuelle Testpläne
go.work        gemeinsamer Workspace für getrennte Go-Module
```

Die Go-Module unter `client/` und `server/` bleiben bewusst getrennt.

## Voraussetzungen

- Go gemäß `client/go.mod` und `server/go.mod`
- Node.js und npm
- Wails 2 für Desktop-Builds
- für produktionsnahe Serverläufe ein zuverlässiges lokales Linux-Dateisystem

## Entwicklung

### Tests

```bash
go test ./client/... ./server/...
go test -race ./client/...
go test -race ./server/...
go vet ./client/... ./server/...

cd client/frontend
npm ci
npm run check
npm test -- --run
npm run build
```

### Desktop-Client

```bash
cd client
wails dev
```

Produktionsnaher lokaler Build:

```bash
cd client
wails build -clean
```

Unter macOS entsteht die App unter `client/build/bin/Remember.app`.

### Server

```bash
cd server
go run ./cmd/remember-server
```

Die Entwicklungsstandards binden nur an `127.0.0.1:8080`. Konfiguration, Dockerbetrieb und öffentliche Routen sind in [`server/README.md`](server/README.md) beschrieben.

## Dokumentation

- [Produktanforderungen](docs/PRD.md)
- [Technisches Design](docs/DESIGN.md)
- [Architekturentscheidungen](docs/adr/)
- [macOS-Manuelltests](docs/MANUAL_TESTS_MAC_CLIENT.md)
- [Änderungshistorie](CHANGELOG.md)

## Sicherheitsgrenzen

- Der Serverprozess terminiert kein TLS und darf nicht direkt öffentlich exponiert werden.
- Öffentliche Registrierung und Recovery sind noch nicht freigegeben; Blob- und Sync-Routen erfordern gültige Sitzungen, Blob-Bytes unterliegen zusätzlich einer Benutzerquota.
- Refresh-Tokens müssen später im Betriebssystem-Schlüsselspeicher des Clients liegen; ein Klartext-Fallback ist nicht zulässig.
- Der Blob-Speicher benötigt ein lokales Dateisystem; Netzwerkdateisysteme werden nicht unterstützt.

Sicherheitsprobleme sollten nicht über öffentliche Issues mit Geheimnissen oder Nutzerdaten gemeldet werden.

## Lizenz

Eine öffentliche Lizenz wurde noch nicht festgelegt. Bis dahin gelten die gesetzlichen Standardrechte; Nutzung oder Weiterverteilung ist nicht automatisch gestattet.
