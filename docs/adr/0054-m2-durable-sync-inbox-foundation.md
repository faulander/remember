# ADR 0054: Dauerhafte Sync-Inbox als Grundlage für fortsetzbaren Pull

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

`LocalCore.SyncOnce` koppelt Pull und Apply derzeit an einen seitenweisen Apply-Plan und stoppt vor Pull, solange irgendeine lokale Outbox-Operation ungelöst ist. Damit bleiben Daten sicher, unbeteiligte Remote-Änderungen können gemäß `SYNC-012` aber noch nicht unabhängig heruntergeladen und später objektbezogen angewendet werden.

Für eine inkrementelle Umstellung muss der Download-Fortschritt zuerst dauerhaft vom bestätigten Apply-Fortschritt getrennt werden, ohne das bestehende Apply-Verhalten vorzeitig zu ändern.

## Entscheidung

Client-Schema v25 ergänzt `sync_inbox_changes` als unveränderliches Journal authentifizierter Pull-Änderungen:

- der Cursor ist positiv und Primärschlüssel, die Operations-ID zusätzlich eindeutig;
- der vollständige `Change`-Payload wird mit strikten Typ-, Mutations-, Revisions-, Namens-, Blob- und Tombstone-Grenzen gespeichert;
- Payload und Ingest-Zeitpunkt sind per Trigger unveränderlich, konfliktbehaftete Inserts einschließlich `INSERT OR REPLACE` werden abgewiesen und Zeilen dürfen nicht gelöscht werden;
- der lokale Zustand schreitet ausschließlich `pending → applying → applied` fort; Zeitstempel und SQL-Trigger verhindern Sprünge und Rückwärtsübergänge;
- `downloaded_cursor` wird bei der Migration einmalig aus `confirmed_cursor` übernommen oder mit null initialisiert.

`Store.IngestPullPage` akzeptiert neue Seiten nur ab dem aktuellen Download-Cursor und nur mit lückenlosen Cursorwerten bis `next`. Die komplette Seite und der neue Download-Cursor committen atomar. Bereits vollständig gespeicherte Bereiche dürfen nur mit byteidentischem Payload wiederholt werden; Teilüberlappungen und Abweichungen schlagen ohne Mutation fehl.

Store-Primitiven lesen Inbox-Zeilen, markieren die beiden Zustandsübergänge, erkennen frühere noch nicht vollständig angewendete Änderungen desselben Objekts und verschieben den bestehenden `confirmed_cursor` ausschließlich über einen zusammenhängenden Präfix bereits angewendeter Inbox-Zeilen.

## Sicherheits- und Durability-Invarianten

1. Download-Fortschritt darf nie vor einer teilweise gespeicherten Seite stehen.
2. Derselbe Cursor beziehungsweise dieselbe Operations-ID darf nie einen anderen Payload bezeichnen.
3. Eine spätere angewendete Zeile darf den bestätigten Cursor nicht über eine frühere pending/applying Zeile hinwegziehen.
4. Inbox-Historie bleibt auch nach Neustart verfügbar und kann nicht still umgeschrieben oder gelöscht werden.
5. Der vorhandene Outbox-, `SyncOnce`- und Apply-Plan-Pfad bleibt in diesem Schnitt unverändert.

## Grenzen und Folgen

Diese ADR ist ausschließlich die Speichergrundlage. `LocalCore.SyncOnce` schreibt Pull-Seiten noch **nicht** in die Inbox und selektiert noch keine objektbezogen unblocked Änderungen. `SYNC-012` bleibt deshalb offen. Ein Folgeschnitt muss Transport-Ingest, Scheduling, bestehende Apply-Pläne und Konfliktisolation verbinden und dabei die Cursor-Invarianten dieser ADR erhalten.

Tests decken Migration, atomare Seitenaufnahme, exakten Replay, Mismatch/Gaps, strikte Shapes, Neustart, SQL-Trigger, monotone Zustände, zusammenhängenden Confirmed-Frontier und Same-Object-Ordering ab.
