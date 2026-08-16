# Remember

Remember ist eine plattformübergreifende Local-first-Anwendung für persönliche Markdown-Notizen, Ordner und Erinnerungen. Normale `.md`-Dateien bleiben die kanonische lokale Datenquelle; ein zentraler Server synchronisiert mehrere eigene Geräte.

> **Status:** frühe Entwicklung, noch kein produktionsreifes Release. Builds sind derzeit unsigniert und werden manuell aktualisiert. Der aktuelle M2-Abdeckungsstand ist in der [expliziten Konfliktmatrix](docs/M2_CONFLICT_MATRIX.md) abgegrenzt; M2 ist noch nicht vollständig abgenommen.

## Aktueller Stand

- lokaler macOS-Desktop-Client mit Wails, Go, Svelte 5 und TypeScript
- echte Markdown-Dateien mit versioniertem YAML-Frontmatter
- verschachtelte Ordner, Tags, Vorschau, Themes und recoverbares Löschen
- lokaler SQLite-Index, Reconcile, Watcher sowie sichtbare Update/Update-, bidirektionale Edit/Delete-, bidirektionale Note-Move/Delete-, divergente und äquivalente Note-/Folder-Move- sowie fehlende Remote-Objekt-, Parent- und Pfadkonfliktkopien; vollständig manifestierte lokale Nested-Folder-DAGs aus nie versuchten Folder-/Note-Creates und linearen Note-Updates werden bei Folder-Create-Kollision, gelöschtem Parent, divergentem Root-Move oder Move/Remote-Delete identitätsgebunden gerettet; bei divergentem Root-Move werden zusätzlich exakt baseline-identische serverbekannte Descendants unter frischen UUIDs geklont, während lokale Descendants UUID und finale Bytes behalten und attempted, verzweigte, lokal geänderte oder typfremde Historien fail-closed bleiben
- modularer Go-Server mit SQLite/WAL und sicherem Blob-Repository
- interner Identity-, Sync-, Sessions- und Devices-Core mit server-provisioniertem Konfliktbereich
- begrenzter HTTP-Transport für Authentifizierung, Sitzungs-/Geräteverwaltung, Blob-Bytes und idempotenten Cursor-Sync
- descriptor-rekursive exakte Subtree-Verifikation auf Darwin/Linux bindet alle manifestierten Pfade, Folder-Device/Inode und Note-Hashes vor lokaler Nested-Folder-Recovery; Windows bleibt dafür fail-closed
- lokaler Index v40 mit crash-sicherer Outbox, exakter Blob-Staging-Ablage, versiegelten rekursiven lokalen und gemischten serverbekannten Recovery-Manifesten, ancestry-gebundener Sync-Inbox, descriptor-gebundener Löschung technischer Bytes und resumierbarem Notiz-/Folder-Apply
- strikter Client-HTTP-Transport und im Desktop verdrahteter manueller Vordergrund-Sync für Notiz-CRUD sowie identitätsgebundene Folder-Create/-Move/-Delete-Operationen; kurzlebige Access-Tokens bleiben im Arbeitsspeicher, rotierende Refresh-Tokens werden ausschließlich in macOS Keychain, Linux Secret Service beziehungsweise Windows Credential Manager gespeichert und beim App-Start vor Verwendung erneuert; der Desktop registriert neue Konten und bestätigt E-Mail-Codes, angemeldete Geräte und Sitzungen sind gruppiert sichtbar, das aktuelle Gerät kann umbenannt und andere Sitzungen einzeln oder samt Gerät widerrufen werden
- automatisierte Mehrgeräte-Konvergenz über echte Login-, Blob- und Sync-HTTP-Routen einschließlich rekursiver Folder-Create-Recovery, rekursivem Folder Delete gegen Remote-Move, gemischter serverbekannter/lokaler Recovery divergenter Root-Folder-Moves, tief verschachtelter Note-Isolation, A/B/Restart/Cold-C, Serverneustart, kaltem History-Bootstrap, Pagination, verlorenen Antworten und dauerhafter Pull-Seiten-Wiederaufnahme
- der vollständige aktive Folder-/Note-Subtree wird bei Folder Delete gegen einen konkurrierenden Remote-Move atomar unter `_Konflikte/Wiederhergestellt` bewahrt: Folder erhalten neue UUIDs, Notes behalten UUID, Tags, benutzerdefiniertes YAML und exakte Bytes, Original-Folder werden deepest-first tombstoned; Post-Frontier-Historie, mehr als 256 Ebenen oder 10.000 Objekte bleiben fail-closed
- lokale Schema-v39-Sync-Inbox mit atomarem Seiten-Ingest, exaktem Crash-Replay und einem begrenzten Scheduler, der bei ungelöstem Outbox-Konflikt unabhängige Note-Update/-Delete-Ketten in bis zu 256 Folder-Ebenen über vollständig ancestry- und baseline-gebundene Einzelpläne anwendet, ohne den bestätigten Präfix zu überspringen. Fehlende und hashinkonsistente Remote-Blobs werden dauerhaft als sichtbare Issues alarmiert und nach Wiederherstellung fortgesetzt; Creates, Moves, Folder-Mutationen und vollständige Objektisolation bleiben für `SYNC-012` offen

Reminder, Recovery und weitere Folder-/Strukturkonflikte folgen in späteren Schnitten.

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
- Öffentliche Registrierung ist nur bei vollständig konfiguriertem SMTPS-Versand und separatem Token-Seal-Schlüssel aktiv; Recovery ist noch nicht freigegeben. Blob- und Sync-Routen erfordern gültige Sitzungen, Blob-Bytes unterliegen zusätzlich einer Benutzerquota.
- Refresh-Tokens liegen ausschließlich im Betriebssystem-Schlüsselspeicher des Clients; ist dieser nicht verfügbar, bleibt Login ohne Klartext-Fallback fail-closed.
- Der Blob-Speicher benötigt ein lokales Dateisystem; Netzwerkdateisysteme werden nicht unterstützt.

Sicherheitsprobleme sollten nicht über öffentliche Issues mit Geheimnissen oder Nutzerdaten gemeldet werden.

## Lizenz

Eine öffentliche Lizenz wurde noch nicht festgelegt. Bis dahin gelten die gesetzlichen Standardrechte; Nutzung oder Weiterverteilung ist nicht automatisch gestattet.
