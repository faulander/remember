# ADR 0055: Sync-Inbox als Spiegel des cursor-geordneten Apply-Pfads

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

ADR 0054 hat `sync_inbox_changes` und den vom `confirmed_cursor` getrennten `downloaded_cursor` eingeführt, die Inbox aber noch nicht mit `LocalCore.SyncOnce` verbunden. Der erste Integrationsschritt soll die persistierten Pull-Payloads und Crash-Fenster im produktiven Pfad erproben, ohne zugleich Apply-Reihenfolge, Outbox-Blockierung oder Konfliktisolation zu ändern.

## Entscheidung

`LocalCore.SyncOnce` verwendet die Inbox zunächst ausschließlich als dauerhaften Spiegel des bestehenden cursor-geordneten Pull-/Apply-Pfads:

1. Beim Start werden Inbox-Zustände bis zum bereits bestätigten Legacy-Cursor nachgeführt.
2. Ein vorhandener Apply-Plan wird vor seiner Wiederaufnahme in die Inbox aufgenommen beziehungsweise dort payload-identisch bestätigt. Das deckt auch Pläne ab, die vor der Inbox-Verdrahtung angelegt wurden.
3. Jede validierte Pull-Seite wird vor `CreateApplyPlan` atomar über `IngestPullPage` persistiert.
4. Der unveränderte Apply-Plan wird weiterhin vollständig und in Cursor-Reihenfolge ausgeführt.
5. Nach erfolgreichem Abschluss werden alle Inbox-Zeilen bis `confirmed_cursor` transaktional und idempotent über `pending → applying → applied` nachgeführt.

`Store.ReconcileInboxAppliedThroughConfirmed` verändert den Legacy-`confirmed_cursor` nicht. Bei Datenbanken ohne jede Inbox-History darf es den älteren `downloaded_cursor` einmalig auf den bereits bestätigten Legacy-Stand nachziehen; dadurch bleiben Läufe zwischen Schema-Einführung und Verdrahtung kompatibel. Sobald Inbox-Zeilen existieren, verifiziert es den tatsächlich vorhandenen Bereich bis `downloaded_cursor` auf Lücken und gespeicherte Payload-/Zustandsinvarianten. Historische Cursor vor der ersten Inbox-Zeile werden nicht nachträglich verlangt, damit Migrationen mit bereits bestätigter History gültig bleiben. Pending- und nach einem früheren Prozessschritt bereits applying markierte Zeilen werden mit monotonen Zeitstempeln abgeschlossen.

## Crash- und Replay-Verhalten

Scheitert der Prozess nach Inbox-Ingest, aber vor oder während Apply, darf `downloaded_cursor` vor `confirmed_cursor` liegen. Der nächste Lauf pullt weiterhin ab `confirmed_cursor`, verlangt für die erneut gelieferte Seite einen exakt identischen Inbox-Payload und setzt anschließend den bestehenden Legacy-Apply fort. Abweichende Replay-Payloads schlagen vor einem neuen Apply-Plan fail-closed fehl.

## Grenzen

Dieser Schnitt implementiert ausdrücklich noch keine objektbezogene Isolation oder Inbox-Scheduling:

- Pull startet weiterhin am `confirmed_cursor`, nicht am `downloaded_cursor`.
- Ein ungelöster Outbox-Konflikt blockiert Pull weiterhin global über `ErrUnresolvedOutbound`.
- Apply-Pläne bleiben seitenweise und strikt cursor-geordnet.
- `SYNC-012` bleibt offen, bis unbeteiligte Objekte trotz gezielt blockierter Änderungen fortschreiten können.

## Verifikation

Store-Tests decken idempotentes und crash-fortsetzbares Nachführen, Teilfrontiers sowie fehlende Inbox-Zeilen ab. App-Tests decken erfolgreiche und paginierte Spiegelung, Fehler nach Ingest mit exaktem Replay, Payload-Mismatch und die Wiederaufnahme eines bereits vorhandenen Apply-Plans ab. Bestehende Integrations-, Race- und Cross-Build-Suiten bleiben unverändert grün.
