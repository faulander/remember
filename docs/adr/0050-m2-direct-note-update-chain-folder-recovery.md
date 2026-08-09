# ADR 0050: Folder-Create-Recovery mit linearen direkten Note-Updates

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

ADR 0043 rettet kollidierende oder parentlose Folder-Creates mit direkten, nie versuchten Note-Creates. Ein lokaler Edit vor dem ersten Sync erzeugt jedoch eine noch vollständig lokale Create→Update-Historie, obwohl nur die final sichtbaren Markdown-Bytes publiziert werden müssen.

## Entscheidung

Client-Schema v23 ergänzt `conflict_folder_create_note_chain_members`. Automatische Recovery wird auf direkte Notes erweitert, deren Historie exakt aus einem pending Create und null oder mehr pending, nie versuchten Updates besteht. Jede Operation besitzt genau eine Vorgängerabhängigkeit; Branches, zusätzliche Abhängigkeiten oder Dependents, Moves, Deletes sowie attempted, replayed oder conflicted Operationen bleiben fail-closed. Nested Folder bleiben ausgeschlossen.

Vor dem Recovery-Journal folgt der Store jede Kette vollständig. Die Update-Operationen werden in derselben SQLite-Transaktion superseded, in einem unveränderlichen ordinalen Manifest gebunden und vor Blob-Cleanup geschützt. Das bestehende Member-Manifest bindet Create-Hash, finalen Hash, Note-ID, Namen und neue Operations-ID. Descriptor-relative Entry-Prüfung akzeptiert weiterhin ausschließlich die manifestierten direkten finalen Note-Dateien und den exakten Folder-Inode.

Nach dem physischen Inode-Move und Trusted Reconcile werden alter Create und sämtliche Updates durch genau einen recovered-root Folder-Create und genau einen finalen Note-Create je Note ersetzt. Jeder Note-Create hängt ausschließlich vom neuen Root ab; UUID, finaler Blob-Hash und exakte Markdown-Bytes bleiben erhalten. Replacement-DAG und Konfliktauflösung werden atomar gespeichert.

## Verifikation

Tests prüfen zwei lokale Updates vor dem ersten Sync, Path-Collision, `parent_unavailable`, Crash und Neustart, superseded Create/Updates, genau einen finalen Replacement-Create, identische finale Bytes/UUIDs sowie A/B- und kalte C-Konvergenz. Negative Tests prüfen attempted Updates, Move, Delete, Branching und unerwartete Verzeichniseinträge. Die bisherigen Empty- und Create-only-Fälle bleiben unverändert erfolgreich.

## Folgen

Direkte Offline-Edits blockieren die bounded Folder-Create-Recovery nicht mehr. Rekursive Folder, Note-Moves/-Deletes und nichtlineare oder bereits versuchte Historien benötigen weiterhin eine allgemeinere Subtree-Transformation und bleiben fail-closed.
