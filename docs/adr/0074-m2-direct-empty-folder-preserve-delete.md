# ADR 0074: Preserve-and-delete für direkte leere Child-Folder

- Status: Angenommen
- Datum: 2026-08-12

Die versionierte Preserve-and-delete-Anfrage v2 erweitert ADR 0070 ausschließlich um aktuell vorhandene direkte Child-Folder, die selbst leer sind. Notes, tiefere Folder und Änderungen nach dem vom Client dauerhaft aufgenommenen `known_cursor` werden abgelehnt. Der kanonische Root-Zustand muss ein Move sein; sein Change sowie die Historie des Roots und seiner direkten Children dürfen die dauerhaft aufgenommene Grenze nicht überschreiten. Version, Grenze und Actor sind Bestandteil des unveränderlichen Replay-Hashes. V1 bleibt für historisch stets leere Folder kompatibel.

Eine SQLite-Transaktion erzeugt zuerst den Recovery-Root und danach frische Child-UUIDs, anschließend tombstoned sie die Original-Children und zuletzt den Original-Root. Der resultierende Change-Log-Abschnitt ist zusammenhängend; das Resultat bindet `first_cursor`, `last_cursor`, `clone_count` und jede Original-/Recovery-/Create-/Delete-Zuordnung. Für `n` Children gilt exakt `last=first+2n+1`; Clone-Creates und Deletes belegen ihre ordinalen Cursorpositionen. SQL-Guards binden Server-Clonezeilen an die exakten Change-/Version-Artefakte und begrenzen das Set durch `clone_count`. Servermigration 007 und Client-Schema v34 bewahren V1-Zeilen vorwärtskompatibel.

Der Client verwendet V2 nur, wenn der exakte kanonische Move bereits in der unveränderlichen Inbox innerhalb des `downloaded_cursor` liegt; andernfalls bleibt der bestehende V1-Slice zuständig. Resolution-ID, Request-Version, Grenze, Spanne und Clone-Mapping überleben Neustarts. Beim Pull werden Recovery-Creates parent-first normal publiziert und exakt gebundene, lokal bereits abwesende Original-Child-/Root-Deletes als erfüllt behandelt.

Nicht unterstützt bleiben Notes, Nested Folder, post-frontier History und rekursive Subtrees.
