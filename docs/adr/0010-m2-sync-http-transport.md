# ADR 0010: Authentifizierter M2-Sync-HTTP-Transport

- Status: Angenommen
- Datum: 2026-08-07

## Kontext

Identity, Sessions, Blob-Bytes und der mandantengebundene Sync-Core sind vorhanden. Für die Zwei-Geräte-Konvergenz benötigen Clients einen öffentlichen, begrenzten Transport für idempotente Operationen und Cursor-Pull, ohne Benutzer- oder Geräteidentitäten aus Requestdaten zu übernehmen.

## Entscheidung

### Autorisierung und Bindung

Der HTTP-Layer authentifiziert ein Access-Token und bindet den Sync-Core ausschließlich über `SyncForActor(principal.UserID, principal.DeviceID)`. Weder JSON noch Query akzeptieren `user_id` oder `device_id`. Inaktive Konten, Geräte oder Sitzungen schlagen generisch als `invalid_session` fehl.

### Routen

- `POST /v1/sync/operations` übermittelt genau eine idempotente Mutation.
- `GET /v1/sync/changes?after=<cursor>&limit=<n>` liest Änderungen nach einem benutzerspezifischen Cursor.

Das Operations-JSON ist auf 16 KiB begrenzt, lehnt unbekannte Felder, weitere JSON-Werte und Content-Type-Parameter ab und verwendet kanonische UUIDv7 sowie kleingeschriebene SHA-256-Hexwerte. Es bildet `operation_id`, `mutation`, `object_id`, `object_type`, `base_revision`, nullable `parent_id`, `name` und nullable `blob_hash` direkt auf die interne Mutation ab. Der Core bleibt für semantische Kombinationen, portable Namen, Parent-Regeln, Kollisionen und Idempotenz zuständig.

Erfolgreiche und identisch wiederholte Operationen liefern `accepted`, `revision` und `cursor`. Fachliche Konflikte bleiben erfolgreiche HTTP-Antworten und liefern einen stabilen Konfliktcode; sie sind keine Transportfehler.

Pull erlaubt ausschließlich die je einmal vorkommenden Parameter `after` und `limit`. Werte sind kanonische vorzeichenlose Dezimalzahlen ohne Pluszeichen oder führende Nullen. Fehlendes `after` bedeutet 0, fehlendes `limit` überlässt dem Core dessen Standard. Die Antwort enthält stabile Versionszustände, `has_more` und `next_cursor`; Parent und Blob bleiben explizit nullable.

### Grenzen und Fehler

Authentifizierung erfolgt bei Pull vor Query- und Bodyvalidierung. Submit und Pull teilen acht concurrency Slots, die erst nach Authentifizierung und billiger Transportvalidierung belegt werden. Überlauf liefert `429` mit `Retry-After`.

Öffentliche Fehler sind begrenzt:

- `invalid_request` (`400`),
- `invalid_session` (`401`),
- `blob_unavailable` (`409`),
- `operation_replay_mismatch` (`409`),
- `rate_limited` (`429`),
- `internal_error` (`500`).

Die allgemeinen Transportfehler `method_not_allowed` (`405`) und `not_found` (`404`) gelten wie für die übrige HTTP-Fläche zusätzlich.

Requestlogs verwenden nur `/v1/sync/operations` beziehungsweise `/v1/sync/changes`. Bodies, Querywerte, Operationen, Objekt-IDs, Hashes und Notiznamen werden nicht protokolliert.

## Explizit nicht enthalten

- Batch-Submit, Streaming, WebSockets oder Server Push,
- Client-Outbox, Apply-Journal und Konfliktmaterialisierung,
- Kompression oder öffentliche Reminder-Endpunkte,
- Tombstone-GC oder Geräte-Acknowledgements,
- verteilte Concurrency-Limits für mehrere Serverprozesse.

## Folgen

Ein authentifizierter Client kann Blob-Bytes vorab hochladen, anschließend Operationen idempotent senden und Änderungen anderer eigener Geräte cursorbasiert abrufen. Die Transportfläche übernimmt keine Konfliktlogik und kann keine Tenant-ID überschreiben. Zwei-Geräte-End-to-End-Konvergenz erfordert als nächsten Schnitt die dauerhafte Client-Outbox und ein crash-sicheres Apply-Journal.
