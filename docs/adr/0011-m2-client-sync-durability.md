# ADR 0011: Clientseitige Sync-Durability und Outbox

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Der Server stellt authentifizierte Blob- und Sync-Transporte bereit. Lokale Markdown-Dateien bleiben kanonisch; deshalb darf ein Absturz zwischen Dateisystembeobachtung, Blob-Upload und Operations-Submit weder Änderungen verlieren noch spätere Dateibytes unter einem älteren Hash versenden.

## Entscheidung

Der lokale Index besitzt ab Schema v2 eine unveränderliche Outbox, bestätigte Objektbaselines, einen bestätigten Pull-Cursor und persistente Apply-Pläne/-Schritte. Migrationen werden sequenziell und transaktional ausgeführt; WAL, Foreign Keys und `synchronous=FULL` sind verpflichtend. Markdown-Inhalte werden nicht in SQLite gespeichert.

Reconcile staged Notizbytes vor dem SQLite-Commit unveränderlich unter `.remember/sync/outbox/<sha256>` (8 MiB, technische Verzeichnisse `0700`, Dateien `0600`, fsync und descriptor-verankerter Symlink-Schutz auf Darwin/Linux). Windows-Sync-Staging schlägt in diesem Schnitt geschlossen fehl, bis alle Komponenten per Handle gegen Reparse-Point-Austausch gesichert und real auf NTFS getestet sind. Danach ersetzt eine lokale Transaktion den beobachteten Snapshot und fügt die daraus abgeleiteten Operationen hinzu. Ein unveränderter erneuter Scan erzeugt keine Operation.

Neue lokale Objekte erzeugen Create, bestätigte Objekte abhängig von ID, Parent, Name und Hash Move/Update/Delete. Kombinierte Move-/Inhaltsänderungen werden als geordnetes Move gefolgt von Update mit vorhergesagter Basisrevision gespeichert. Creates sind Eltern-vor-Kind, Deletes Kind-vor-Eltern. Nur nie versuchte Pending-Absichten werden durch eine neue unveränderliche Operation superseded; versuchte oder finale Historie wird nicht umgeschrieben.

Actor- und Operations-IDs bleiben UUIDv7. Objekt- und Parent-IDs akzeptieren jede kanonische, von Nil verschiedene RFC-4122-UUID, damit bestehende UUIDv4-Frontmatter-IDs synchronisierbar bleiben.

Ein neu initialisierter Root darf seine gültigen Objekte als Creates erfassen. Ein Upgrade eines vorhandenen v1-Index setzt dagegen `bootstrap_required`; erst `clientsync.PrepareBootstrap` übernimmt den aktuellen gültigen Baum explizit. Indexverlust-/Folder-Recovery bootstrapped niemals automatisch geratenen Zustand.

Apply-Pläne und Schritte werden crash-sicher persistiert. Ein begrenzter Executor führt Remote-Create, -Update, -Move und recoverable -Delete für Notizen aus: Der vollständige Plan wird vor jeder Dateisystemmutation validiert, Blobs werden auf Größe und SHA-256 geprüft und müssen die erwartete Identität bereits in den authentifizierten, unveränderten Bytes tragen; Veröffentlichungen erfolgen descriptor-verankert. `prepared -> applying`, `pending -> applied` und die abschließende Cursor-/Baseline-Transaktion sind monoton und wiederholbar. Nach jeder Veröffentlichung baut Reconcile den Index neu auf; Outbox-Erfassung wird ausschließlich für die erwartete Objekt-ID und nur bei exakt passendem SHA-256 unterdrückt. Andere oder konkurrierend veränderte Bytes werden weiterhin als lokale Absicht erfasst. Beim Öffnen mit aktivem Plan bleibt der letzte persistierte Snapshot unangetastet, bis der Executor veröffentlichte Remote-Bytes von konkurrierenden Offline-Änderungen unterschieden hat; allgemeine Reconcile-/Watcher-Läufe bleiben bis dahin gesperrt. ADR 0013 ergänzt identitätsgebundene Folder-Creates; ADR 0014 ergänzt inode-gebundene Folder-Moves/-Deletes. Windows bleibt bis zur handle-sicheren Reparse-Point-Implementierung ebenfalls geschlossen.

## Nicht enthalten

- HTTP-Client, Authentifizierung oder Tokenablage in Keychain/Credential Manager/Secret Service,
- Scheduler, Retry/Backoff oder automatischer Blob-/Operations-Upload,
- Konfliktmaterialisierung oder Hintergrund-Sync,
- UI für Konto, Sync-Fortschritt oder Konflikte,
- Bereinigung nicht mehr referenzierter lokaler Staging-Blobs, Geräte-Acknowledgements und Tombstone-GC.

## Folgen

Ausgehende Absichten und die dazugehörigen exakten Bytes überleben Neustarts. Eingehende Notiz-Creates/-Updates/-Moves/-Deletes können nach Abstürzen idempotent fortgesetzt werden; Cursor und Baselines wechseln erst nach vollständig angewendetem Plan. ADR 0012 verbindet Outbox, Blob-Resolver und Pull mit dem HTTP-Transport; weitere Schnitte ergänzen die sicher fehlenden Mutationsarten.
