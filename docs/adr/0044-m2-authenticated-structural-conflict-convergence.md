# ADR 0044: Authentifizierte Strukturkonflikt-Konvergenz

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Die einzelnen Note-Move/Delete- und Folder-Create-Recovery-Zustandsautomaten sind umfangreich mit dem In-Memory-Remote getestet. Für den M2-Abschluss müssen dieselben Semantiken zusätzlich gemeinsam mit realer HTTP-Serialisierung, tokenabgeleiteter Geräteidentität, Server-SQLite, Blobtransport und Cursor-Historie belegt werden.

## Entscheidung

Der produktionsnahe Integrationstest ergänzt ein eigenes authentifiziertes Strukturkonflikt-Szenario mit drei unabhängigen Clientroots und drei über `/v1/auth/login` erzeugten Geräten.

Über echte Blob- und Sync-Routen werden nacheinander geprüft:

1. lokaler Note-Move samt abhängigem Edit gegen kanonischen Remote-Delete,
2. lokaler Note-Delete gegen kanonischen Remote-Move,
3. Folder-Create-Pfadkollision mit einem streng manifestierten direkten Note-Create gemäß Schema v19,
4. Konvergenz beider aktiver Geräte,
5. vollständiger kalter History-Bootstrap eines dritten Geräts.

Der Test verlangt:

- Abwesenheit aller ursprünglichen, verlierenden und kanonisch tombstoned Note-Pfade,
- sichtbare Konfliktkopien mit den erwarteten vollständigen Inhalten auf allen Geräten,
- identische relative Recovery-Pfade des Folder-Unterbaums,
- Erhalt der ursprünglichen direkten Note-UUID im Frontmatter auf allen drei Geräten.

## Folgen

Die zentralen Strukturkonflikte sind nicht mehr nur gegen das In-Memory-Protokoll, sondern gemeinsam mit den produktionsnahen Auth-, Session-, Blob-, Sync- und HTTP-Grenzen verifiziert. Weitere Folder-Move/Delete- und rekursive Subtree-Zellen werden demselben Harness hinzugefügt, sobald ihre lokalen Zustandsautomaten abgeschlossen sind.
