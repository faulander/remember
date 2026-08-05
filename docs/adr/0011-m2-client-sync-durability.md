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

Apply-Pläne und Schritte werden bereits crash-sicher persistiert, aber in diesem Schnitt noch nicht auf das Dateisystem ausgeführt.

## Nicht enthalten

- HTTP-Client, Authentifizierung oder Tokenablage in Keychain/Credential Manager/Secret Service,
- Scheduler, Retry/Backoff oder automatischer Blob-/Operations-Upload,
- tatsächliche Ausführung eines Apply-Plans, Konfliktmaterialisierung oder Remote-Blob-Download,
- UI für Konto, Sync-Fortschritt oder Konflikte,
- Bereinigung nicht mehr referenzierter lokaler Staging-Blobs, Geräte-Acknowledgements und Tombstone-GC.

## Folgen

Ausgehende Absichten und die dazugehörigen exakten Bytes überleben Neustarts und können später deterministisch übertragen werden. Der nächste Schnitt führt Apply-Pläne mit descriptor-sicheren Dateisystemoperationen aus und verbindet Outbox/Pull mit dem HTTP-Transport.
