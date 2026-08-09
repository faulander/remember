# ADR 0051: Direkte Notes im Folder-Move gegen Remote-Delete

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann einen remote bereits vorhandenen, ursprünglich leeren Folder lokal verschieben und darin offline Notes anlegen oder editieren, während ein anderes Gerät die ursprüngliche Folder-ID löscht. Der kanonische Tombstone muss gewinnen, ohne den neuen lokalen Subtree zu verlieren.

## Entscheidung

Client-Schema v24 erweitert das monotone `conflict_folder_move_delete_recoveries`-Journal um eigene, unveränderliche Note-Member- und Update-Chain-Manifeste. Zulässig sind ausschließlich direkte Note-Creates, die vom konflikthaften Folder-Move abhängen, mit optionaler linearer Kette nie versuchter pending Updates. Nested Folder, Move/Delete, Branches, disconnected aktive Operationen, attempted Zustände und unerwartete Einträge bleiben fail-closed.

Vor und nach dem physischen Move wird der exakte Folder-Inode descriptor-relativ gegen die vollständige direkte Entry-Liste, Note-UUIDs und finale Hashes geprüft. Ein unerwarteter Zielzustand wird mit demselben Inode an den versuchten Pfad zurückgestellt. Trusted Reconcile ersetzt nur die tombstoned Original-Root-ID durch eine neue Recovery-ID; Note-UUIDs bleiben gleich.

Nach bestätigter exakter Tombstone-Baseline und bestätigtem Recovery-Parent werden alter Create→Update-Verlauf, neuer Root-Create und finale Note-Creates atomar umgeschrieben. Jede Note hängt ausschließlich vom neuen Root-Create ab. SQL-Guards binden Konflikt, Move-Intent-Inode, Manifest, lineare Topologie und Replacement-DAG. Manifestierte Chain-Blobs bleiben bis zum Abschluss von Cleanup ausgeschlossen. Windows bleibt fail-closed.

## Verifikation

Tests prüfen Create plus zwei Updates, exakte finale Bytes/Hashes, Crash nach Move/Reconcile, Neustart, Original-Tombstone, neue Folder-UUID, erhaltene Note-UUID und A/B/Cold-C-Konvergenz. Negative Tests prüfen Nested Folder, attempted Create, Branch, disconnected Operation und unerwarteten Eintrag. Der leere ADR-0045-Pfad bleibt unverändert abgedeckt.

## Folgen

Folder-Move/Remote-Delete bewahrt nun leere Folder und den kleinsten streng beweisbaren direkten Note-Subtree. Allgemeine rekursive Subtrees bleiben separat.
