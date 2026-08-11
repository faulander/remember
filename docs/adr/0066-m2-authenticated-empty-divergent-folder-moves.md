# ADR 0066: Authentifizierte Abnahme leerer divergenter Folder-Moves

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

ADR 0065 implementiert die leere root-level Zelle mit In-Memory-Transport und Fault-Injection. Für die freigegebene Produktregel fehlte die zusammengesetzte Mehrgeräte-Abnahme über echte Identity-, Sync- und Pull-Pfade.

## Entscheidung und Nachweis

`TestAuthenticatedEmptyDivergentFolderMovesConverge` verwendet den realen Integrationserver und drei authentifizierte Geräte:

1. A und B teilen den leeren Folder F mit derselben UUID.
2. A verschiebt F nach `Server`, B offline nach `Local`.
3. B bewahrt den exakten lokalen Device/Inode unter `_Konflikte/Wiederhergestellt` mit neuer Folder-UUID und übernimmt den kanonischen Serverpfad mit der ursprünglichen UUID.
4. Der unveränderlich vorab gebundene Replacement-Create wird über die echten Sync-Routen publiziert.
5. Nach B-Neustart bleiben beide Identitäten stabil.
6. A und ein kalter Client C konvergieren auf exakt dieselbe kanonische und wiederhergestellte UUID; weder `F` noch der unterlegene Root-Pfad `Local` bleiben bestehen.

Die bestehenden Fault-Boundary-Tests aus ADR 0065 bleiben für die einzelnen Journalphasen maßgeblich.

## Folgen

Die leere root-level Divergent-Move-Zelle ist über A/B/Restart/Cold-C produktionsnah abgenommen. Nichtleere, nested und different-parent Folder bleiben bis zu eigenen vollständigen Manifest- und DAG-Schnitten fail-closed.
