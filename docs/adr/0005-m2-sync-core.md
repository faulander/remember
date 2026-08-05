# ADR 0005: Interner M2-Sync-Core

- **Status:** Akzeptiert
- **Datum:** 2026-08-03
- **Bezug:** technische Grundlagen für `SYNC-003`–`SYNC-005`, Konflikterkennung für `SYNC-006`–`SYNC-010` und Fortsetzung für `SYNC-012`

## Scope

Dieser Schnitt implementiert ausschließlich den mandantengebundenen serverseitigen Sync-Core. HTTP, Authentifizierung, Sessions, E-Mail, Blob-Byte-Speicherung und Client-Sync folgen separat.

## Mandantengrenze

`Service.ForActor(userID, deviceID)` erzeugt einen gebundenen Actor-Service. Dessen Submit- und Pull-Methoden akzeptieren keine überschreibbare Benutzer- oder Geräte-ID. Jede Operation prüft in derselben Datenbanktransaktion ein aktives Konto und ein aktives, diesem Konto gehörendes Gerät.

## Persistenz

- Actor- und Operations-IDs sind UUIDv7. Objekt- und Parent-IDs akzeptieren jede kanonische, von Nil verschiedene RFC-4122-UUID (einschließlich bestehender UUIDv4-Frontmatter-IDs); alle IDs werden als 16-Byte-BLOB gespeichert.
- `devices` bindet Geräte an Benutzer.
- `content_blobs` ist ein global deduplizierbares Verfügbarkeitsregister für bereits dauerhaft gespeicherte SHA-256-Inhalte; `user_content_blobs` erteilt die nicht beobachtbare mandantenspezifische Referenzberechtigung.
- `sync_objects` enthält den aktuellen Zustand mit zusammengesetztem Mandanten-Primärschlüssel und partieller Eindeutigkeit aktiver `(parent, name_key)`-Pfade.
- `sync_object_versions` ist unveränderlich.
- `sync_operations` speichert kanonischen Request-Hash, vorgeschlagene Absicht und akzeptiertes oder fachliches Konfliktergebnis.
- `sync_change_log` verwendet pro Benutzer monotone Cursor; akzeptierte Mutationen erzeugen genau einen Eintrag.

## Operationen

Unterstützt werden `create`, `update`, `move` und `delete`:

- Create verlangt Basisrevision 0 und eine noch nie verwendete Objekt-ID.
- Update ändert ausschließlich den Inhalt einer Notiz.
- Move ändert Parent und Namen; Ordnerverschiebungen dürfen keine Zyklen erzeugen.
- Delete erzeugt eine Tombstone-Revision; nichtleere Ordner werden abgelehnt.
- Objekttypen sind unveränderlich.
- Aktive Notizen benötigen einen als verfügbar registrierten SHA-256-Blob; Ordner dürfen keinen Blob tragen.

Portable Namen verwenden NFC, Windows-kompatible Zeichenregeln und Unicode Case Folding entsprechend dem Client. Vollständige Pfade einschließlich verschobener Unterbäume bleiben auf 768 UTF-8-Bytes begrenzt; die reservierten Root-Namen `.remember` und `_Konflikte` werden nicht als normale Sync-Objekte akzeptiert. Parent `NULL` wird für die Pfadeindeutigkeit durch einen internen Null-UUID-Schlüssel repräsentiert.

## Idempotenz und Konflikte

Die Operations-ID ist pro Benutzer eindeutig. Eine identische Wiederholung liefert das gespeicherte Ergebnis. Dieselbe ID mit anderer kanonischer Absicht liefert `ErrOperationReplayMismatch`.

Gültige Konkurrenz wird als Operation ohne Change-Log-Eintrag gespeichert. Codes:

- `object_exists`
- `object_missing`
- `object_deleted`
- `base_revision_mismatch`
- `parent_unavailable`
- `path_collision`
- `folder_not_empty`
- `folder_cycle`
- `type_mismatch`

Ungültige Eingaben, inaktive Actors und nicht verfügbare Blobs sind Go-Fehler und erzeugen keine Operation. Zustandsänderungen verwenden zusätzlich bedingte Revision-Updates.

## Pull

Pull ist actor-gebunden, aufsteigend nach Cursor und liefert unveränderliche Versionszustände einschließlich Tombstones. Standardlimit ist 100, Maximum 500; `limit + 1` bestimmt `has_more`. Es gibt in diesem Schnitt kein Ack und keine Bereinigung.

## Abgrenzung der Anforderungsabdeckung

Dieser Core schließt Meilenstein 2 nicht ab. Konflikte werden hier nur verlustfrei erkannt und als Absicht persistiert; die Materialisierung von Konfliktkopien und Wiederherstellungsobjekten folgt mit der vollständigen Konflikt-Engine. Blob-Bytes, serverseitige Hashprüfung und Integritätsalarme folgen im Blob-Repository. Client-Outbox, Apply-Journal, Transport, Sessions und Zwei-Geräte-Konvergenztests sind ebenfalls spätere M2-Schnitte. Entsprechend gelten `SYNC-006`–`SYNC-010`, `SYNC-013` und `M2-AC-001`–`M2-AC-004` durch diese ADR allein ausdrücklich nicht als erfüllt.
