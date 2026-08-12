# ADR 0070: Server-Core für atomare Preserve-and-Delete-Recovery leerer Folder

- Status: Angenommen
- Datum: 2026-08-12

## Kontext

ADR 0069 definiert die vollständige rekursive Preserve-and-Delete-Operation. Als erster eng beweisbarer Schnitt wird nur ein aktuell leerer kanonischer Folder unterstützt. Nichtleere Subtrees bleiben bis zur vollständigen DAG-Klonimplementierung fail-closed.

## Entscheidung

Servermigration 006 und `ActorService.PreserveAndDeleteEmptyFolder` speichern eine actor-gebundene immutable Resolution. Der Request bindet UUIDv7 der Resolution, ursprüngliche Konfliktoperation, Folder-UUID und exakt persistierte kanonische Revision.

In einer SQLite-Transaktion werden Actor, ursprünglicher Folder-Delete-`base_revision_mismatch`, Tenant/Gerät, aktuelle Revision und aktuelle Leere geprüft. Anschließend erzeugt der Server einen neuen leeren Folder unter `ConflictRecoveredID`, schreibt normale kanonisch gehashte Operation/Version/Change-Zeilen und tombstoned das Original mit genau geprüftem konditionalem Update. Recovery-Create liegt im Cursor vor dem Original-Delete. Die abgeschlossene Resolution bindet per Foreign Keys Konfliktoperation, Recovery-Objekt und beide Cursor.

Replay derselben Resolution liefert exakt dieselben IDs/Cursor; abweichende Payload ergibt `ErrOperationReplayMismatch`. Recovery-Namen werden UTF-8-sicher begrenzt und bei aktiver Kollision deterministisch gesuffixt. Nichtleere, fremde, stale oder nicht exakt konfliktgebundene Folder werden ohne Teilzustand mit `ErrPreserveDeleteUnavailable` abgelehnt.

## Folgen

Der atomare Protokollkern ist für leere Folder vorhanden. HTTP-Transport, Client-Resolution und rekursives Clone/Tombstone bleiben offen. Erst die rekursive Erweiterung erfüllt die volle ADR-0069-Zelle.
