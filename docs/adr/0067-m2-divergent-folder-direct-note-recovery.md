# ADR 0067: Direkte Note-Ketten in divergent verschobenen Foldern

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

ADR 0065/0066 bewahren nur leere divergente root-level Folder. Ein offline lokal verschobener, ursprünglich leerer Folder kann danach direkte neue Notes und lokale Edits enthalten.

## Entscheidung

Schema v31 erweitert ausschließlich diese Zelle. Zulässig sind direkte lokal erstellte Notes ohne Server-Baseline mit vollständigen, nie versuchten linearen Create→Update-Ketten. Immutable Member- und Chain-Tabellen binden Note-UUID, ursprüngliche und neue Operations-IDs, Namen, Create-/Final-Hashes, Reihenfolge und Abhängigkeiten. Branches, zusätzliche/disconnected Operationen, attempted Zustände, Move/Delete, Nested Folder, unbekannte Dateien und unvollständige Indexmanifeste bleiben fail-closed.

Vor und nach der Inode-Bewegung prüft `VerifyRootedSubtreeExpected` den vollständigen direkten Subtree descriptor-retained gegen exakte Namen, Typen und finale Hashes. Trusted Reconcile bindet Notes und Recovery-Folder ohne neue lokale Operationen. Erst nach exakt angewendeter kanonischer Inbox-Move-Revision werden alte Create-/Update-Ketten atomar superseded und Replacement-Creates mit denselben Note-UUIDs und finalen Hashes abhängig vom neuen Recovery-Folder-Create eingereiht. Blob-Cleanup hält finale Chain-Bytes bis zu diesem atomaren Ersatz fest. Vollständige Topologie wird in derselben Transaktion vor `evacuated` und vor Completion erneut geprüft; SQL-Views verhindern direkte Zustandsübergänge ohne exakte Replacement-Artefakte. Folder-Mutation-Echos im reservierten Bereich sind ausschließlich als Source=Target für die exakt per UUID und Recovery-Pfad gebundene abgeschlossene Recovery zulässig.

## Verifikation

`TestSyncOnceRecoversEditedDirectNoteInDivergentRootFolderMove` prüft finale Bytes, Frontmatter-UUID, Tags im byteidentischen Dokument, erhaltenen Folder-Inode, Restart und Abschluss ohne ungelöste Outbox. `TestSyncOnceRejectsUnsafeDivergentDirectNoteRecoveries` deckt Branch, attempted Create, Nested Folder und unindexierte Datei ohne Recovery-Mutation ab. ADR-0065-Empty- und Fault-Boundary-Tests bleiben kompatibel.

## Folgen

Nichtlineare, serverbekannte, bewegte/gelöschte oder rekursive Inhalte bleiben ausdrücklich außerhalb dieses Schnitts. Eine authentifizierte A/B-/Cold-C-Abnahme folgt separat.
