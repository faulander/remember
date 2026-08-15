# ADR 0077: Rekursives Folder Preserve-and-delete

- Status: Angenommen
- Datum: 2026-08-13

## Entscheidung

Protokoll v4 erweitert ADR 0075 auf den vollständigen aktiven Folder-Subtree am bestätigten `known_cursor`. Alle Folder erhalten unter `_Konflikte/Wiederhergestellt` frische UUIDs; der Recovery-Root erhält zusätzlich einen kollisionsfreien Namen. Aktive Notes behalten UUID, Name, Blob-Hash und exakte Markdown-Bytes und werden in derselben Servertransaktion unter den jeweils geklonten Parent verschoben. Danach werden die ursprünglichen Folder deepest-first tombstoned, der Original-Root zuletzt. V1-, v2- und v3-Request-/Response-Shapes, Hashes und Replays bleiben unverändert.

Die deterministische Cursorfolge lautet Recovery-Root-Create, übrige Folder-Clones parent-first, Note-Moves nach Parent-Reihenfolge, übrige Folder-Deletes deepest-first und Root-Delete. Für `f` geklonte Descendant-Folder und `n` verschobene Notes gilt `last = first + 2f + n + 1`. Das Ergebnis bindet jeden Folder an Quell- und Ziel-Parent, Tiefe, Quellrevision, Name sowie Create-/Delete-Cursor und jede Note an Quell- und Ziel-Parent, Quell-/Zielrevision, Name, Hash und Move-Cursor. Servermigration 010 und Client-Schema v37 versiegeln Root, Actor/Device, Request-Hash, Konflikt- und Resolution-Operation, Bounds, Counts sowie alle Deskriptoren vor dem Abschluss.

Vor einer Mutation konstruiert der Server die aktive Topologie am Frontier und prüft sie gegen den aktuellen Zustand. Rekursive Post-Frontier-Ancestry erfasst auch zwischenzeitlich angehängte und wieder entfernte Descendants; jede Änderung in diesem Kandidatensatz lehnt die Operation atomar ab. Zyklen, Parent-/Tiefenabweichungen, mehr als 256 Folder-Ebenen, mehr als 10.000 Objekte einschließlich Root, fehlende Blob-Berechtigung beziehungsweise Blob-Bytes und Canonical Drift bleiben `preserve_delete_unavailable`.

Der Client akzeptiert v4 nur über den 32-MiB-gebundenen Decoder und validiert die vollständige bijektive Parent-DAG, Reihenfolge, Tiefen, IDs, Revisionen, Hashes und Cursorformel vor Persistenz. Apply erzeugt Folder parent-first, materialisiert oder verschiebt Notes anhand der versiegelten UUID/Bytes, tombstoned Folder child-first und setzt jeden Schritt crash-fortsetzbar fort. Eine bereits vorbereitete v1-v3-Resolution bleibt replay-exakt; Promotion auf v4 ist nur nach authentifizierter expliziter `preserve_delete_unavailable`-Antwort mit neuer Resolution-Operations-ID zulässig. Mehrdeutige Fehler ändern die Version nicht. Windows bleibt fail-closed.

## Folgen

Die rekursive ADR-0069-DAG ist für aktive Folder-/Note-Subtrees umgesetzt. Die pauschale Fresh-ID-Regel aus ADR 0069 bleibt für Folder bestehen, wird für Notes aber wie in ADR 0075 bewusst durch identitätserhaltende Moves ersetzt.

Server-, Migration-, Transport-, Client-Apply- und authentifizierte Integrationstests prüfen einen gemischten dreistufigen Baum, exakte Mappings, V1–V3-Kompatibilität, Replay, verlorene Antworten, Fault-Boundaries, Depth-/Count-Grenzen, Post-Frontier-Historie, fehlende Blobs sowie A/B/Restart/Cold-C-Konvergenz mit exakten Note-UUIDs und Bytes.
