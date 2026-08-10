# ADR 0062: Dauerhafte Blob-Integritätsalarme

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein fehlender oder hashinkonsistenter Remote-Blob stoppte Apply und Cursor bereits fail-closed, war nach dem zurückgegebenen Fehler aber nicht als dauerhafter, sichtbarer Zustand erfasst. Damit war M2-AC-004 nur teilweise erfüllt.

## Entscheidung

Schema v29 speichert `sync_integrity_incidents`, exakt an Apply-Plan, Inbox-Cursor, Objekt-ID und erwarteten Blob-Hash gebunden. Unterstützte Codes sind `missing_blob` und `hash_mismatch`. Wiederholungen derselben Cursor-/Code-Kombination erhöhen einen Zähler und aktualisieren den letzten Zeitpunkt, statt Alarme zu vervielfachen.

Der HTTP-Transport übersetzt `blob_not_found` in den typisierten Fehler `ErrBlobMissing`. Der Apply-Preflight persistiert diesen Fehler sowie Größen-/Hashabweichungen vor seiner Rückgabe. Plan, Inbox und Confirmed-Cursor bleiben unverändert und fortsetzbar. SQLite läuft mit `recursive_triggers=ON`, damit auch implizite Deletes durch `INSERT OR REPLACE` die No-delete- und Provenienz-Guards nicht umgehen können. `LocalCore.IntegrityIncidents` liefert offene Alarme; der Desktop-State spiegelt sie als sichtbare Issues `sync_missing_blob` beziehungsweise `sync_hash_mismatch`.

## Verifikation

Tests prüfen Plan-/Inbox-/Objekt-/Hash-Bindung, deduplizierte Wiederholungen, Sichtbarkeit über `LocalCore`, beide Fehlercodes, unveränderten Confirmed-Cursor, pending Inbox und ausbleibende Dateipublikation.

## Folgen

M2-AC-004 besitzt nun einen dauerhaften lokalen Alarmzustand statt nur eines transienten Fehlers. Eine spätere Bedienoberfläche für Acknowledge/Retry kann auf derselben Tabelle aufbauen; automatisches Quittieren findet nicht statt.
