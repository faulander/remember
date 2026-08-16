# ADR 0080: Divergente Folder-Moves mit gebundenen Ziel-Parents

- Status: Angenommen
- Datum: 2026-08-13

## Kontext

ADR 0064 bis ADR 0068 und ADR 0079 lösen divergente Folder-Moves nur dann automatisch, wenn lokales und kanonisches Ziel Root-Pfade sind. Bei einem verschachtelten Move enthält der authentifizierte Konfliktsnapshot zwar die kanonische Parent-UUID, aber nicht deren lokalen Pfad oder Dateisystemidentität. Eine Veröffentlichung allein anhand eines aktuellen Pfad-Lookups könnte deshalb nach Parent-Move, Parent-Austausch oder lokaler Mutation in den falschen Subtree schreiben.

## Entscheidung

1. Die bestehende ADR-0079-Identitätsregel bleibt unverändert: Der kanonische Server-Subtree behält Root- und serverbekannte Descendant-UUIDs sowie exakte bestätigte Bytes. Die lokale Verlustfassung liegt unter `_Konflikte/Wiederhergestellt`; serverbekannte Descendants erhalten dort frische UUIDs, lokale Descendants behalten UUID und finale Bytes.
2. Divergente Folder-Moves dürfen Root-, gleiche verschachtelte oder unterschiedliche verschachtelte Ziel-Parents besitzen. Lokales und kanonisches Ziel werden jeweils als vollständiger relativer Pfad persistiert. Der kanonische Parent stammt ausschließlich aus dem authentifizierten Konfliktsnapshot; der lokale Parent ausschließlich aus der gebundenen Outbox-Mutation und dem aktuellen, dazu passenden Indexobjekt.
3. Für jeden nichtleeren Ziel-Parent wird die vollständige Ancestor-Kette vom Root-Folder bis zum unmittelbaren Parent versiegelt. Jede Zeile bindet Seite, Tiefe, Objekt-ID, Parent-ID, relativen Pfad, bestätigte Revision und Operation sowie Folder-Device/Inode. Alle Ancestors müssen serverbekannte Folder ohne offene lokale Mutation sein. Die Kette darf den divergenten Folder nicht enthalten und ist auf 256 Ebenen begrenzt.
4. Vor Evakuierung, kanonischer Publication und Abschluss werden Parent-Manifest, aktuelle Baselines, Indexidentitäten und descriptor-relativ gelesene Folder-Inodes erneut geprüft. Fehlender, verschobener, ersetzter, lokal mutierter oder revisionsveränderter Parent stoppt fail-closed, bevor ein weiterer Zustandsübergang beziehungsweise eine Replacement-Outbox entsteht.
5. Der vorhandene Konfliktsnapshot enthält die erforderliche kanonische Parent-UUID. Deshalb bleibt das Sync-Wire-Protokoll unverändert. Ein noch nicht lokal vorhandener kanonischer Parent wird nicht geraten: Der Konflikt bleibt zunächst ungelöst, der authentifizierte Pull darf den Parent materialisieren, und erst ein späterer Zyklus darf mit vollständiger Bindung planen.
6. Bereits unter Schema v40 vorbereitete Root-Recoveries besitzen keine Parent-Zeilen. Sie bleiben replay-kompatibel, weil beide gebundenen Parent-IDs `NULL` sind und beide Zielpfade Root-Komponenten bleiben. Neue Recoveries schreiben immer ein versiegeltes Parent-Manifest, auch wenn beide Parent-Ketten leer sind.
7. Windows bleibt ohne handle-basierte, Reparse-Point-sichere Folder-Publication fail-closed.

## Grenzen

- Lokale neue, attempted oder intent-belegte Ziel-Parents bleiben ausgeschlossen.
- Eine Ancestor-Kette mit fehlender Baseline, Typabweichung, Zyklus, mehr als 256 Foldern oder abweichender Pfad-/Inode-Bindung bleibt fail-closed.
- Diese Entscheidung führt keine allgemeine Parent-Merge-Semantik ein und ändert keine Serverobjekte außerhalb der bereits akzeptierten kanonischen Move-Historie.

## Folgen

Schema v41 ergänzt ein dauerhaft versiegeltes Parent-Manifest und bindet die Parent-UUIDs direkt an die Recovery-Zeile. Die bestehende kanonische Staging-/Recovery-Pipeline kann dadurch verschachtelte Ziele verwenden, ohne Pfade aus ungebundenem aktuellem Zustand abzuleiten. Der zusätzliche Aufwand ist linear in der Tiefe beider Ziel-Parents und bleibt durch zweimal 256 Ancestors begrenzt.
