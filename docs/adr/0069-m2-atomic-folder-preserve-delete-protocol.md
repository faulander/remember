# ADR 0069: Atomare Preserve-and-Delete-Operation für Folder Delete gegen Remote-Move

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

Ein lokaler Folder-Delete kann nach einem konkurrierenden Remote-Move mit `base_revision_mismatch` abgelehnt werden. Der Client besitzt dann weder einen revisionsgebundenen vollständigen Remote-Subtree noch einen Beweis, dass später keine Kinder oder Moves hinzugekommen sind. Ein Client-only-Fallback kann Inhalte verlieren oder den Pull wegen aktiver Kinder unter einem lokal fehlenden Parent blockieren.

ADR 0064 legt deshalb eine servergestützte, verlustfreie Lösung fest. Der Server arbeitet bereits mit genau einem SQLite-Writer und kann Objektzustand, Subtree und Cursor in einer Transaktion binden.

## Entscheidung

Der Server erhält eine actor-gebundene idempotente Operation `PreserveAndDeleteFolder` mit:

- neuer UUIDv7 `resolution_operation_id`;
- UUID des zuvor konfliktbehafteten Delete-Requests;
- ursprünglicher Folder-UUID;
- exakt erwarteter kanonischer Revision aus dem unveränderlich gespeicherten Konfliktergebnis.

In **einer** SQLite-Transaktion:

1. Replay wird über Mandant und `resolution_operation_id` mit kanonischem Request-Hash geprüft.
2. Der ursprüngliche Request muss ein authentifiziertes Folder-Delete desselben Nutzers/Geräts mit `base_revision_mismatch` und byteidentisch persistierter kanonischer Revision sein.
3. Die aktuelle ursprüngliche Folder-UUID muss noch aktiv sein und exakt diese Revision besitzen. Andernfalls entsteht ein neues sichtbares `canonical_changed`; der Client darf nicht raten.
4. Der Server liest den vollständigen aktuell aktiven Subtree descriptor-unabhängig aus seiner relationalen Parent-Topologie. Maximal 10.000 Objekte und 256 Ebenen sind zulässig; größere Subtrees bleiben sichtbar fail-closed.
5. Unter `ConflictRecoveredID` wird ein neuer Recovery-Root mit serverseitig erzeugter UUIDv7 und deterministischem Konfliktnamen angelegt. Folder-Descendants erhalten neue UUIDv7 und werden in die Recovery-DAG abgebildet. **Revision durch ADR 0075:** Notes behalten ihre UUID sowie exakten Blob-Bytes/-Hash und werden atomar in den zugehörigen Recovery-Folder verschoben; sie werden nicht geklont oder rekeyed.
6. Danach werden die Original-Folder child-first tombstoned. Jeder Folder-Clone, Note-Move und Folder-Delete erhält eine eigene unveränderliche Objektversion und Cursorzeile. Die gesamte Cursor-Spanne wird gemeinsam mit einer versiegelten heterogenen Mapping-Menge gespeichert.
7. Commit erfolgt nur, wenn Clone-DAG, Blob-Referenzen, Pfad-Eindeutigkeit, Tombstones und Resolution vollständig geschrieben sind. Jeder Fehler rollt alles zurück.

Der Server liefert Recovery-Root-ID und zusammenhängende Cursor-Spanne. Replay liefert exakt dasselbe Ergebnis. Die Operation ist ausschließlich für den Eigentümer-Tenant zulässig; Nutzer-/Geräteidentität stammt aus der Session, nie aus dem Body.

## Client-Semantik

Der Client darf die Resolution nur für den exakt gebundenen ungelösten Folder-Delete-Konflikt anfordern. Danach zieht er ausschließlich den normalen Change-Log:

- Folder-Clone-Creates erscheinen parent-first unter `_Konflikte/Wiederhergestellt`;
- Note-Moves behalten UUID und Blob unverändert;
- Original-Folder-Deletes erscheinen child-first;
- `confirmed_cursor` rückt nur nach vollständig angewendetem Präfix vor;
- lokaler bereits fehlender Original-Subtree wird als idempotent gelöscht behandelt;
- es gibt keinen lokalen Snapshot, keine Pfadrekonstruktion und keine automatische Textmutation.

## Sicherheits- und Betriebsgrenzen

- Die Resolution akzeptiert keine fremden Objekt-, Parent-, Blob- oder Ziel-IDs.
- Reservierte Namen und Zielpfade werden serverseitig erzeugt.
- Größen-/Tiefenlimits verhindern ungebundene Transaktionen; Rate-Limits des Sync-Transports gelten zusätzlich.
- Aktive Pfadkollision am Recovery-Ziel erzeugt einen deterministischen Suffix innerhalb derselben Transaktion.
- Windows bleibt für Client-Apply rooted fail-closed, bis Reparse-Point-Sicherheit handle-basiert vorhanden ist.

## Folgen

Die bisher protokoll-blockierte Zelle besitzt ein verlustfreies Protokolldesign. Implementierung erfolgt zuerst im Server-Core mit Replay-, Rollback-, Tenant- und Grenztests, danach als strikter HTTP-Transport und schließlich mit crash-fortsetzbarem Client-Apply sowie A/B/Cold-C-Abnahme.
