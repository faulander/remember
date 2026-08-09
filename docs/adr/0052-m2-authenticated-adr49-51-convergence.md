# ADR 0052: Authentifizierte Konvergenz für ADR 0049 bis 0051

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Die erweiterten Strukturzellen aus ADR 0049 bis ADR 0051 waren durch fokussierte In-Memory-Core-Tests abgedeckt. Für die M2-Abnahme fehlte ein gemeinsamer Nachweis über echte Login-, Blob- und Sync-HTTP-Routen, voneinander getrennte Geräte und Roots sowie einen kalten Bootstrap.

## Entscheidung

`TestAuthenticatedADR49To51Convergence` verwendet drei getrennte Login-Sitzungen und Client-Roots gegen `server/integrationtest`. Der Test deckt in einem begrenzten Ablauf ab:

- divergente Note-Moves innerhalb derselben verifizierten Parent-ID: Die kanonische Original-ID bleibt am Gewinnernamen mit exakten Gewinnerbytes; der verlierende Move samt abhängigem Edit erscheint genau einmal mit neuer UUID und byteidentisch auf allen Geräten in `_Konflikte/Wiederhergestellt`,
- eine Folder-Create-Pfadkollision mit direkter Note und zwei nie versuchten Updates: Der Folder erhält unter dem reservierten Recovery-Parent eine neue UUID, während Note-UUID und finale Markdown-Bytes erhalten bleiben,
- einen lokalen Move eines bestehenden Folders gegen dessen Remote-Delete mit direkter Note und zwei nie versuchten Updates: Die ursprüngliche Folder-ID bleibt als exakter Tombstone erhalten; der gerettete Folder erhält eine neue UUID, Note-UUID und finale Bytes bleiben erhalten.

Kanonische IDs, Parent-Beziehungen, Revisionen, Tombstones und Blob-Hashes werden ausschließlich aus authentifiziertem HTTP-Pull bestimmt. Der Test greift nicht auf Serverinternas zu. A und B konvergieren aktiv; C bootstrapped die vollständige Historie kalt.

## Verifikation

Der Test fordert eindeutige Kandidaten für ursprüngliche und gerettete Folder, exakt einen sichtbaren divergenten Note-Konflikt, unveränderte beziehungsweise bewusst neu vergebene UUIDs, exakte Markdown-Bytes auf A/B/C, den Tombstone mit Revision 2 und die reservierte Parent-ID. `SyncOnce` wird nur in statisch begrenzten Schleifen aufgerufen.

## Folgen

Die in ADR 0049 bis ADR 0051 implementierten Zellen besitzen nun zusätzlich zur Core-Abdeckung einen gemeinsamen produktionsnahen Auth-/Blob-/Sync-HTTP-Nachweis. Reale Windows-/Linux-Dateisystemtests und allgemeinere rekursive Subtrees bleiben davon unberührt.
