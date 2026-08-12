# ADR 0068: Authentifizierte Abnahme divergenter Folder mit direkten Note-Ketten

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

ADR 0067 erweitert die divergente root-level Folder-Move-Recovery auf lokal erstellte direkte Notes mit linearen, nie versuchten Create→Update-Ketten. Die Abnahme verwendete bislang den In-Memory-Transport.

## Entscheidung und Nachweis

`TestAuthenticatedDivergentFolderDirectNotesConverge` prüft über echte Login-, Blob- und Sync-Routen:

1. A und B teilen den zunächst leeren Folder F.
2. B verschiebt F offline nach `Local`, erstellt dort eine Note und editiert sie; A verschiebt F kanonisch nach `Server`.
3. B bewahrt den exakten Folder-Inode, die Note-UUID, finalen Frontmatter-/Body-Bytes und Tags unter dem neuen Recovery-Folder.
4. Alte Create-/Update-Operationen werden atomar durch Folder-/Note-Creates mit exakten Abhängigkeiten ersetzt. Eine redundante lokale Recovery-Folder-Beobachtung wird nur bei exakt passender abgeschlossener Recovery und Create-Abhängigkeit vor dem Enqueue unterdrückt; reservierte Pfade werden nicht allgemein freigegeben.
5. B-Restart erhält Inode, Folder-UUID, Note-UUID und Bytes.
6. A und ein kalter Client C konvergieren auf die ursprüngliche UUID am kanonischen Pfad sowie dieselbe neue Recovery-Folder-UUID und Note-UUID. Alte Root-Pfade fehlen auf allen Geräten.

## Folgen

Der ADR-0067-Scope ist produktionsnah über A/B/Restart/Cold-C abgenommen. Branches, attempted Historien, serverbekannte Notes, Move/Delete, Nested Folder, unbekannte Einträge und rekursive Subtrees bleiben fail-closed.
