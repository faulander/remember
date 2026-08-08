# ADR 0021: Verlustfreie Note-Create-Pfadkollisionen

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei offline arbeitende Clients können unterschiedliche Notizen mit verschiedenen UUIDs am selben portabel normalisierten Pfad erstellen. Der Server akzeptiert die erste Create-Operation und lehnt die zweite mit `path_collision` ohne kanonischen Objektzustand für deren UUID ab. Der verlierende Client muss seine lokalen Bytes retten und zugleich Platz für den authentifizierten Gewinner aus dem Pull schaffen.

## Entscheidung

Ausschließlich ein Note-Create mit Konfliktcode `path_collision` und `canonical: null` wird unterstützt. Der Client sichert die exakten lokalen Bytes hashadressiert, erzeugt eine neue Konfliktnotiz-UUID und transformiert die Kopie mit Herkunftsmetadaten. Weil kein kanonischer Zustand der abgelehnten UUID existiert, ist `canonical_revision: 0` nur für diesen Konfliktgrund zulässig.

Vor dem Pull wird die ursprüngliche Datei descriptor-relativ und hashgebunden in den deterministischen technischen Konflikt-Trash verschoben. `MoveRootedExpected` nimmt einen Absturz auch aus seinem versteckten Move-Staging wieder auf. Eine Pfadersetzung wird niemals als zu evakuierende Quelle übernommen.

Die sichtbare Konfliktkopie wird erst veröffentlicht, nachdem der reservierte Wiederherstellungsordner dauerhaft bestätigt ist und am ursprünglichen Pfad ein anderes Notizobjekt liegt, dessen UUID, Parent, Name und SHA-256 exakt zu einer aktuellen Sync-Baseline und einem angewendeten Schritt eines abgeschlossenen Pull-Plans passen. Zusätzlich werden die tatsächlichen Frontmatter-Bytes geprüft. Ein lediglich lokal erzeugter oder veränderter Pfadersatz kann diese Schranke nicht erfüllen.

Nach Reconcile ist die gerettete Kopie eine normale Create-Operation unter `_Konflikte/Wiederhergestellt`; die abgelehnte ursprüngliche Create-Operation bleibt unveränderliche Konflikthistorie.

## Folgen

Konkurrierende Note-Creates am selben Pfad konvergieren zu dem serverseitig akzeptierten Gewinner am Originalpfad und einer sichtbaren, synchronisierten Konfliktkopie aller verlierenden Bytes. Move-Kollisionen, Folder-Kollisionen und Kollisionen ohne sicheren lokalen Quellbezug bleiben separat zu behandeln.
