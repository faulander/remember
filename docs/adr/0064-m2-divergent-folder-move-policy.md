# ADR 0064: Verlustfreie Politik für divergente Folder-Moves

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

Bei konkurrierenden Folder-Moves derselben UUID zu verschiedenen Zielen persistiert der Server genau einen kanonischen Zielzustand. Der Client behandelte die abweichende lokale Fassung bislang fail-closed. Für den M2-Abschluss wurde eine eindeutige, verlustfreie Produktregel benötigt.

Die Gegenrichtung lokaler Folder-Delete gegen Remote-Move ist nicht äquivalent: Ein punktuell leerer lokaler Ordner beweist nicht, dass der kanonische Remote-Subtree keine späteren Kinder oder Moves enthält. Ein reiner Client-Fallback wurde deshalb bereits als unsicher verworfen.

## Entscheidung

1. Bei divergentem Folder Move/Move gewinnt der kanonische Serverpfad für die ursprüngliche Folder-UUID.
2. Der lokal unterlegene Subtree wird sichtbar unter `_Konflikte/Wiederhergestellt` bewahrt.
3. Der Recovery-Root erhält eine neue Folder-UUID. Weitere Identitäten dürfen nur nach einer vollständigen, revisions- und DAG-gebundenen Manifestprüfung übernommen oder ersetzt werden.
4. Der erste Implementierungsschnitt ist ausschließlich ein root-level, leerer Folder mit exakt gebundenem Device/Inode. Nichtleere, verschachtelte, different-parent oder lokal weiterveränderte Fälle bleiben fail-closed, bis eigene Schnitte sie beweisbar abdecken.
5. Evakuierung, Index-Reconcile, kanonischer Apply und Ersatz-Create werden dauerhaft journalisiert und crash-fortsetzbar. Vor dem Zustand `evacuated` muss jeder Fehler den exakt verschobenen Inode samt Indexidentität zum lokalen Ausgangspfad zurückführen.
6. Folder Delete gegen Remote-Move bleibt fail-closed, bis eine atomare servergestützte Preserve-and-Delete-Operation oder ein gleichwertiger revisionsgebundener Subtree-Snapshot definiert ist. Ein Client darf diese Zelle nicht anhand lokaler Momentaufnahme auflösen.
7. Windows bleibt für rooted Recovery fail-closed, bis handle-sicherer Reparse-Point-Schutz real implementiert und getestet ist.

## Folgen

Die Produktregel ist festgelegt, M2 wird aber nicht durch Dokumentation künstlich verengt. Die Implementierung darf nun inkrementell vom leeren Root-Folder zu exakt manifestierten direkten Note-Ketten erweitert werden. Automatische rekursive Recovery bleibt verboten, solange vollständige Topologie, Source-Restoration und descriptor-retained Manifestprüfung nicht gemeinsam bewiesen sind.
