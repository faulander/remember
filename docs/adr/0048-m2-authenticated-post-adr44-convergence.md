# ADR 0048: Authentifizierte Konvergenz der Strukturzellen nach ADR 0044

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Die nach ADR 0044 implementierten Zellen waren mit dem In-Memory-Sync-Remote abgedeckt: leerer Folder-Move gegen Remote-Delete sowie äquivalente Root- und Nested-Note-Moves. Für die M2-Abnahme muss derselbe Ablauf über echte Login-, Blob- und Sync-HTTP-Routen sowie unabhängige Geräte und Roots nachgewiesen werden.

## Entscheidung

Ein produktionsnaher Integrationstest komponiert `server/integrationtest` mit drei separaten Login-Sitzungen und Client-Roots A, B und C. Er prüft in einem begrenzten Ablauf:

- den authentifizierten Remote-Tombstone einer Folder-ID gegen einen lokalen, Inode-erkannten Move,
- die sichtbare Recovery des leeren Folders unter neuer UUID direkt in `_Konflikte/Wiederhergestellt`,
- äquivalente konkurrierende Root-Note-Moves mit abhängigem lokalem Edit,
- äquivalente konkurrierende Nested-Note-Moves unter einem bekannten Folder mit abhängigem lokalem Edit,
- das Ausbleiben unnötiger Note-Konfliktkopien,
- aktive Konvergenz von A und B sowie einen kalten vollständigen Bootstrap von C.

Die kanonischen Folder-IDs und Tombstone-Zustände werden aus dem authentifizierten HTTP-Pull bestimmt, nicht aus Testzugriffen auf Serverinternas. Der lokale Folder-Move wird wie ein Watcher-Hinweis über einen expliziten Move-Candidate reconciled.

## Verifikation

`TestAuthenticatedPostADR44Convergence` prüft ursprüngliche Tombstone-ID, neue Recovery-ID, exakten reservierten Recovery-Parent und sichtbaren Pfad. Für beide Note-Zellen werden unveränderte Note-UUIDs, die finalen abhängigen Bytes und null zusätzliche `.md`-Konfliktkopien auf A, B und C geprüft.

## Folgen

Die Zellen aus ADR 0045 bis ADR 0047 besitzen nun zusätzlich zur fokussierten Core-Abdeckung eine gemeinsame produktionsnahe Drei-Geräte-HTTP-Abnahme. Reale Windows-/Linux-Dateisystemtests bleiben davon unberührt.
