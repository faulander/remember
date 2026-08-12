# ADR 0071: HTTP-Transport für leere Folder Preserve-and-Delete

- Status: Angenommen
- Datum: 2026-08-12

`POST /v1/sync/folder-preserve-delete` exponiert ausschließlich den actor-gebundenen ADR-0070-Core. Der Body enthält Resolution-Operations-ID, ursprüngliche Konflikt-Operations-ID, Folder-ID und erwartete kanonische Revision. Nutzer und Gerät stammen ausschließlich aus dem Access-Token.

Der Transport verwendet striktes JSON, UUIDv7 für Operations-IDs, portable Objekt-ID-Prüfung, positive begrenzte Revisionen, den bestehenden Sync-Concurrency-Slot und generische Fehler. `preserve_delete_unavailable` und Replay-Mismatch werden als 409 geliefert. Die Antwort enthält nur Recovery-Folder-ID und die zusammenhängenden Create-/Delete-Cursor.

Transporttests prüfen Principal-Bindung, exaktes DTO-Mapping und Antwortform. Nichtleere Subtrees bleiben im Core fail-closed; der Transport erweitert den fachlichen Scope nicht.
