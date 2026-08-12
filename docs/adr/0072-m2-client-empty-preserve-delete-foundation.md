# ADR 0072: Clientfundament für leere Preserve-and-Delete-Resolution

- Status: Angenommen
- Datum: 2026-08-12

Client-Schema v32 persistiert eine unveränderlich an den lokalen Folder-Delete-`base_revision_mismatch` gebundene Resolution mit eigener UUIDv7, Folder-ID und exakt gespeicherter kanonischer Revision. Nach erfolgreichem ADR-0071-HTTP-Aufruf werden Recovery-Folder-ID und die zwei zusammenhängenden Cursor atomar als `resolved` gespeichert.

Der Remote-HTTP-Client implementiert striktes Request-/Response-Mapping für `/v1/sync/folder-preserve-delete`, einschließlich UUID-/Cursor-Prüfung und generischer Konfliktfehler.

Dieser Schnitt ruft die Resolution noch nicht automatisch aus `SyncOnce` auf. Client-Apply muss zunächst beweisen, dass der lokal bereits gelöschte leere Folder den serverseitig erzeugten Recovery-Create und Original-Tombstone crash-fortsetzbar über den normalen Change-Log konsumiert. Nichtleere Folder bleiben serverseitig fail-closed.
