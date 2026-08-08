# ADR 0026: Generationsgebundene Bereinigung finaler Outbox-Blobs

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Create- und Update-Operationen halten ihre exakten Markdown-Bytes content-addressed unter `.remember/sync/outbox/<sha256>` vor. Nach akzeptierten, superseded oder vollständig materialisierten Konfliktoperationen werden diese Upload-Bytes nicht mehr für Replay benötigt. Ohne Cleanup wächst die private Ablage mit jeder Notizversion. Derselbe Inhalts-Hash kann später erneut in einer anderen Operation auftreten und muss dann erneut gestaged und nach deren Abschluss erneut bereinigt werden können.

## Entscheidung

Schema v10 ergänzt das unveränderliche Journal `sync_blob_cleanups` mit dem zusammengesetzten Schlüssel aus SHA-256 und `through_sequence`. Eine Cleanup-Generation umfasst alle finalen Referenzen dieses Hashes bis zur höchsten Outbox-Sequenz. Eine spätere Operation mit demselben Hash erzeugt nach ihrem Abschluss eine neue Generation statt durch den älteren Journaleintrag dauerhaft ausgeschlossen zu werden.

Ein Hash ist nur bereinigbar, wenn mindestens eine zugehörige Note-Create/-Update-Operation akzeptiert, superseded oder als Konflikt vollständig materialisiert ist und keine pending, attempted oder Replay-Mismatch-Operation denselben Hash benötigt. Ebenso darf keine unvollständige Konfliktmaterialisierung den Hash als Quellbytes referenzieren. Dieselben Bedingungen werden unmittelbar vor dem Journaleintrag innerhalb einer SQLite-Transaktion erneut geprüft.

Darwin/Linux validieren den ausschließlich lowercase-kodierten Hashpfad, Modus `0600`, Größe, SHA-256 und Inode descriptor-relativ. Jede Generation verwendet einen eigenen Sentinel `<hash>.cleanup-<through_sequence>` und wird über den geöffneten Descriptor getruncated und fsynct. So kann der kanonische Hashpfad später erneut erstellt und unabhängig in einer weiteren Generation bereinigt werden. Windows bleibt fail-closed.

Cleanup läuft beim Öffnen, am Beginn eines Sync-Zyklus, nach Submit sowie nach Konfliktpublikation/Pull-Apply. Ein Absturz vor dem Journaleintrag wiederholt dieselbe Generation idempotent.

## Folgen

Finale Upload-Blobs belegen nicht dauerhaft bis zu 8 MiB je Version. Mehrfach verwendete identische Inhalte bleiben für jede aktive Operation verfügbar und werden danach generationengenau bereinigt. Zurück bleiben ausschließlich kleine leere Sentinels und unveränderliche SQLite-Auditzeilen ohne Markdown-Inhalt.
