# ADR 0012: Begrenzter Client-HTTP-Transport und Vordergrund-Sync

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Outbox, Apply-Journal sowie die authentifizierten Blob- und Sync-Routen existieren. Ein Netzwerkabbruch nach dem Submit darf eine als `attempted` markierte Operation nicht dauerhaft blockieren oder unter einer neuen Identität wiederholen. Refresh-Tokens dürfen ohne Betriebssystem-Schlüsselspeicher nicht lokal persistiert werden.

## Entscheidung

`clientsync.Store.ListReady` liefert abhängigkeitssichere `pending`- und `attempted`-Operationen. `MarkAttempted` ist idempotent und erhält den ersten Zeitstempel. Nach einem mehrdeutigen Transportfehler bleibt die unveränderliche Operation deshalb mit derselben UUID wiederholbar.

`client/internal/remotehttp` implementiert ausschließlich Blob-PUT/-GET, Operation-Submit und Cursor-Pull. Ein Access-Token wird pro Aufruf aus einer flüchtigen `AccessTokenSource` bezogen und niemals vom Transport gespeichert. Produktionsziele müssen HTTPS verwenden; HTTP ist nur für explizite Loopback-Testserver zulässig. Redirects werden nicht verfolgt, damit Bearer-Tokens nicht weitergereicht werden. JSON, IDs, Hashes, Content-Types und Größen werden strikt begrenzt und Fehler enthalten weder Token noch Response-Body.

`LocalCore.SyncOnce` ist ein manueller, begrenzter Vordergrundzyklus. Er setzt zuerst aktive Apply-Pläne fort, reconciled lokale Dateien, lädt für Notiz-Create/-Update die exakt gestagten Bytes vor dem Submit hoch und wiederholt mehrdeutige Submits mit derselben Operations-ID. Pull beginnt erst ohne ungelöste Outbox-Zustände. Eingehende Seiten müssen lückenlose Cursor und ausschließlich Notiz-Create/-Update/-Move/-Delete enthalten; referenzierte Parent-Ordner müssen lokal bereits eindeutig verfügbar sein; sie werden vor Cursorfortschritt über das crash-sichere Apply-Journal ausgeführt. Pro Aufruf sind höchstens 32 Pull-Seiten erlaubt.

## Nicht enthalten

- Refresh, Login-UI oder Persistenz von Access-/Refresh-Tokens,
- Hintergrund-Scheduler, Retry-Timer, Backoff oder Betriebssystem-Schlüsselspeicher,
- Pull-Apply für Ordner sowie Notizen unter noch nicht lokal verfügbaren Parent-Ordnern,
- automatische Konfliktmaterialisierung und allgemeine Zwei-Geräte-Konvergenz.

## Folgen

Ein Vordergrundaufruf kann bestehende ausgehende Mutationen crash-sicher übertragen und Notiz-Create/-Update/-Move/-Delete anwenden. Netzwerkambiguität strandet keine Operation mehr. Die Produktintegration bleibt bewusst auf flüchtige Access-Tokens und den nachweisbar unterstützten Apply-Teil beschränkt.
