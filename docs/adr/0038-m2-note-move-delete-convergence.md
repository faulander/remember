# ADR 0038: Konvergenz konkurrierender Note-Moves und -Deletes

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann eine Note offline verschieben, während ein anderes Gerät dieselbe Identität löscht. In umgekehrter Reihenfolge kann ein lokaler Delete auf eine inzwischen remote verschobene Note treffen. V1 darf weder verschobene Bytes verlieren noch einen wirksamen Tombstone rückgängig machen.

## Entscheidung

Der Delete gewinnt für die ursprüngliche Note-ID. Die erhaltene Move-Fassung wird unter neuer UUID als sichtbare Konfliktnotiz in `_Konflikte/Wiederhergestellt` synchronisiert.

### Lokaler Move gegen kanonischen Delete

Ein `Move` mit `object_deleted` und authentifiziertem kanonischem Note-Tombstone verwendet die bestehende crash-resumierbare Konfliktmaterialisierung:

1. Die exakten lokalen Bytes werden hashgebunden technisch gestaged.
2. Die lokal verschobene Datei wird unter `.remember/trash/conflicts/<operation>.md` evakuiert und die alte ID gezielt aus dem lokalen Snapshot entfernt.
3. Der kanonische Delete-Apply darf die bereits fehlende Datei nur dann als lokal erfüllt behandeln, wenn Konfliktzustand, Revision, Parent, Name, Blobhash und die exakten Evakuierungsbytes zum Remote-Change passen.
4. Nach dauerhafter Tombstone-Projektion wird die Konfliktkopie unter neuer UUID veröffentlicht und synchronisiert.
5. Staging- und Evakuierungsbytes werden erst nach verifizierter sichtbarer Kopie hashgebunden entfernt.

`HasEvacuatingConflict` schützt den Zeitraum zwischen Staging, Evakuierung und Reconcile auch über Neustarts.

### Lokaler Delete gegen kanonischen Move

Ein stale lokaler Delete mit `base_revision_mismatch` rettet die authentifizierten kanonischen Blobbytes unter neuer UUID. Anschließend wird der Delete auf die höhere kanonische Revision rebased und bleibt von der neuen Konfliktkopie abhängig. Damit bleibt die verschobene Fassung sichtbar erhalten, während die ursprüngliche ID tombstoned konvergiert.

## Verifikation

Tests decken beide Reihenfolgen ab, einschließlich Absturz direkt nach Evakuierung beziehungsweise unmittelbar vor dem Delete-Rebase, Neustart, wiederholtem Sync, zweitem Gerät und kaltem Drittgerät. Sie prüfen Bytes, neue UUID, Tombstone-Revision, ausbleibende Wiederbelebung und vollständige Bereinigung technischer Evakuierungen.

## Folgen

Die Note-Move/Delete-Zelle der M2-Konfliktmatrix ist verlustfrei und deterministisch. Die entsprechende Folder-Zelle bleibt separat, da dort ein vollständiger Unterbaum statt einer einzelnen Datei erhalten werden muss.
