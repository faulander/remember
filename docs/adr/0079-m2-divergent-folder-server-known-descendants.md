# ADR 0079: Divergente Folder-Moves mit serverbekannten Descendants

- Status: Angenommen
- Datum: 2026-08-13

## Kontext

ADR 0064 bis ADR 0068 lösen divergente Root-Folder-Moves nur für leere oder ausschließlich lokal neue direkte Notes. ADR 0076 erweitert die lokale Verlustfassung auf nie versuchte rekursive Create/Update-DAGs, schließt aber serverbekannte Descendants aus. Ein serverbekannter Descendant kann nicht mit derselben UUID zugleich im kanonischen Server-Subtree und in der sichtbaren lokalen Verlustfassung existieren.

## Entscheidung

1. Der kanonische Server-Folder behält seine UUID, seinen serverseitigen Zielpfad und alle serverbekannten Descendant-UUIDs. Seine Descendants müssen lokal exakt ihrem bestätigten Baseline-Zustand entsprechen und dürfen keine offenen lokalen Mutationen besitzen.
2. Der lokale Verlust-Subtree wird vollständig unter `_Konflikte/Wiederhergestellt` sichtbar erhalten. Sein Root erhält wie bisher eine neue Folder-UUID.
3. Jeder serverbekannte Descendant wird in der Verlustfassung geklont: Folder und Notes erhalten frische UUIDs. Bei Notes werden ausschließlich die `remember.note_id` und die dadurch unvermeidliche YAML-Serialisierung geändert; Body, Tags und unbekannte YAML-Felder bleiben semantisch erhalten. Die ursprüngliche Note mit ursprünglicher UUID und ursprünglichen Bytes bleibt im kanonischen Subtree.
4. Ausschließlich lokal neue Descendants behalten ihre UUID und ihre exakten finalen Bytes. Ihre nie versuchten linearen Create/Update-Operationen werden durch parent-first Create-Operationen im Recovery-DAG ersetzt.
5. Das versiegelte Manifest bindet für jedes Mitglied Source- und Recovery-UUID, Parent-Zuordnung, relativen Pfad, Tiefe, Typ, Source-Operation, Replacement-Operation, Baseline-Revision, Folder-Device/Inode sowie Source-, Recovery- und Create-Hashes. Maximal 256 Ebenen und 10.000 Mitglieder sind zulässig.
6. Vor der ersten sichtbaren Dateisystemmutation wird der kanonische, ausschließlich serverbekannte Subtree in einem nonce- und inode-gebundenen privaten Publication-Root aufgebaut. Erst danach wird der lokale Root evakuiert und werden die serverbekannten Notes der Recovery-Fassung umgeschlüsselt.
7. Publication und Resume bleiben monoton: versiegeltes Manifest, vollständiges kanonisches Staging, Recovery-Evakuierung, kanonische Publication, Reconcile beider Identitätsräume, Replacement-Outbox. Vor jedem Zustandsübergang werden Manifest, Dateihashes, Folder-Inodes, Baselines, Abhängigkeiten und Index-Bindings erneut geprüft.
8. Der Server erhält keine Sonderroute. Die geklonte Verlustfassung wird nach lokaler Konfliktauflösung über normale versionierte Folder-/Note-Creates veröffentlicht. Replay, verlorene Antworten und ein kalter Client verwenden die bestehenden Operation-ID- und Cursor-Invarianten.

## Grenzen

- Lokale Updates, Moves oder Deletes an serverbekannten Descendants bleiben fail-closed. Sie benötigen eine eigene Regel für die Aufteilung von kanonischem Zustand und Verlustfassung.
- Versuchte, verzweigte, replay-mismatch- oder bereits konfliktbehaftete lokale Descendant-Historien bleiben fail-closed.
- Serverbekannte Descendants mit fehlender Baseline-Operation, fehlendem Blob, unbekannter Identität, abweichender lokaler Topologie oder abweichenden Bytes bleiben fail-closed.
- Windows bleibt ohne handle-basierte, Reparse-Point-sichere Folder-Publication fail-closed.

## Folgen

Der kanonische Identitätsraum bleibt eindeutig und unverändert. Gleichzeitig bleibt die vollständige lokale Ansicht sichtbar, ohne serverbekannte UUIDs doppelt zu verwenden. Der Preis sind neue UUIDs und eine deterministische Frontmatter-Neuserialisierung für die serverbekannten Recovery-Notes sowie ein größeres, dauerhaft versiegeltes Client-Manifest. Die Regel ist absichtlich enger als eine allgemeine rekursive Merge-Semantik.

## Umsetzung

Client-Schema v40 persistiert das versiegelte gemischte Manifest in `conflict_folder_divergent_tree_*`. `LocalCore.recoverDivergentFolderMoveTree` erstellt vor der Evakuierung ein privates kanonisches Staging, schreibt serverbekannte Recovery-Notes mit frischer UUID um, reconciliert Recovery- und Canonical-Identitätsräume getrennt und erzeugt danach die parent-first Replacement-Outbox. Ein wartender Recovery-Zustand überspringt den allgemeinen Reconcile bis zum nächsten authentifizierten Pull, damit verwaltete Recovery-Objekte nicht als lokale Creates erfasst werden.

`TestSyncOnceRecoversDivergentFolderMoveWithServerKnownDescendants`, `TestDivergentFolderTreeRecoveryFaultBoundaries` und `TestAuthenticatedDivergentFolderTreeConvergesAcrossRestartAndColdClient` belegen gemischte Identitäten, exakte kanonische und lokale Bytes, umgeschlüsselte serverbekannte Recovery-Notes, Restart, A/B und kalten C. `TestSyncOnceRejectsDivergentFolderTreeWithKnownDescendantIntent` hält lokale Intents auf serverbekannten Descendants fail-closed; `TestAuthenticatedDivergentFolderTreeRejectsAdvancedKnownBaseline` belegt dieselbe Sperre für eine nach dem Seal fortgeschriebene, am Ende bytegleiche H→H2→H-Baseline.
