# M2-Konfliktmatrix – Audit nach ADR 0051

Stand: Client-Schema v25, einschließlich der noch nicht verdrahteten Sync-Inbox-Grundlage aus ADR [0054](adr/0054-m2-durable-sync-inbox-foundation.md).

Diese Datei ist die explizite Abgrenzung des aktuellen M2-Stands. Sie ersetzt keine ADR-Entscheidung und erklärt M2 **nicht** für abgeschlossen.

## Statuslegende

- **Konvergiert** – der unterstützte Zustand ist crash-resumierbar implementiert und automatisiert getestet.
- **Fail-closed** – der Client verändert den strittigen Benutzerzustand nicht automatisch und stoppt sichtbar. Das ist eine bewusste Sicherheitsgrenze, keine Konvergenz.
- **Protokoll-blockiert** – eine verlustfreie Entscheidung ist mit dem aktuellen Konfliktsnapshot bzw. den vorhandenen Einzeloperationen nicht beweisbar; Clientcode allein reicht nicht.
- **Extern** – reale Zielhardware oder Betriebsinfrastruktur ist erforderlich.

## Notizen

| Lokale Mutation | Remote-Konkurrenz / Serverkonflikt | Status | Nachweis und Grenze |
|---|---|---|---|
| Create | anderer Create am selben portablen Pfad / `path_collision` | **Konvergiert** | ADR [0021](adr/0021-m2-note-create-path-collision.md); `LocalCore.stageNoteCanonicalAbsentConflict`; `TestSyncOnceMaterializesNoteCreatePathCollision`. |
| Create | Parent remote nicht verfügbar / `parent_unavailable` | **Konvergiert** | ADR [0028](adr/0028-m2-note-parent-unavailable-rescue.md); `LocalCore.stageNoteCanonicalAbsentConflict`; `TestSyncOnceRescuesNoteCreateUnderUnavailableParent`. |
| Create | dieselbe UUID existiert bereits / `object_exists` | **Fail-closed** | ADR [0041](adr/0041-m2-object-exists-fail-closed.md); `TestSyncOnceFailsClosedForCreateObjectExists`. Identitäten werden nicht geraten oder überschrieben. |
| Create/Update/Move/Delete | kanonischer Typ weicht ab / `type_mismatch` | **Fail-closed** | ADR [0037](adr/0037-m2-type-mismatch-fail-closed.md); `TestSyncOnceFailsClosedForNoteToFolderTypeMismatch`, `TestSyncOnceFailsClosedForFolderToNoteTypeMismatch`. |
| Update | konkurrierendes Update / `base_revision_mismatch` | **Konvergiert** | ADR [0017](adr/0017-m2-note-update-conflict-materialization.md); `LocalCore.stageNoteUpdateConflict`; `TestSyncOnceMaterializesConcurrentNoteUpdate`. Beide Fassungen bleiben erhalten, kein Text-Merge. |
| Update | Remote-Delete / `object_deleted` | **Konvergiert** | ADR [0019](adr/0019-m2-edit-delete-conflict-materialization.md); `LocalCore.stageNoteUpdateConflict`; `TestSyncOnceMaterializesEditAgainstRemoteDelete`. Tombstone gewinnt, lokale Bytes werden gerettet. |
| Delete | Remote-Update oder Remote-Move / `base_revision_mismatch` | **Konvergiert** | ADR [0020](adr/0020-m2-delete-edit-conflict-rebase.md) und [0038](adr/0038-m2-note-move-delete-convergence.md); `LocalCore.stageNoteDeleteConflict`, `Store.CompleteConflictMaterializationAndRebaseDelete`; `TestSyncOnceRebasesLocalDeleteAgainstRemoteEdit`, `TestSyncOnceRebasesLocalDeleteAgainstRemoteMove`. |
| Move | Remote-Delete / `object_deleted` | **Konvergiert** | ADR [0038](adr/0038-m2-note-move-delete-convergence.md); `LocalCore.stageNoteUpdateConflict`; `TestSyncOnceRecoversLocalMoveAgainstRemoteDelete`. |
| Move | anderer Pfad belegt / `path_collision` | **Konvergiert** | ADR [0022](adr/0022-m2-note-move-path-collision.md); `LocalCore.stageNoteMovePathCollision`; `TestSyncOnceMaterializesNoteMovePathCollision`. |
| Move | Remote-Objekt fehlt / `object_missing` | **Konvergiert** | ADR [0024](adr/0024-m2-object-missing-move-delete-resolution.md); `LocalCore.stageNoteCanonicalAbsentConflict`; `TestSyncOnceMaterializesMoveForMissingRemoteNote`. |
| Update | Remote-Objekt fehlt / `object_missing` | **Konvergiert** | ADR [0023](adr/0023-m2-note-update-object-missing.md); `LocalCore.stageNoteCanonicalAbsentConflict`; `TestSyncOnceMaterializesUpdateForMissingRemoteNote`. |
| Delete | Objekt fehlt oder ist bereits tombstoned / `object_missing`, `object_deleted` | **Konvergiert** | ADR [0024](adr/0024-m2-object-missing-move-delete-resolution.md) und [0040](adr/0040-m2-idempotent-stale-deletes.md); `Store.ResolveMissingDelete`; `TestSyncOnceTreatsMissingRemoteDeleteAsSatisfied`, `TestSyncOnceTreatsAlreadyDeletedObjectsAsSatisfied`. |
| Move | äquivalenter Remote-Move, Root oder bekannter Parent | **Konvergiert** | ADR [0046](adr/0046-m2-equivalent-root-note-moves.md), [0047](adr/0047-m2-equivalent-nonroot-note-moves.md); `LocalCore.resolveEquivalentNoteMove`, `Store.ResolveEquivalentNoteMove`; Root-/Nested-Tests ab `TestSyncOnceResolvesEquivalentRootNoteMoves`. |
| Move | divergenter Remote-Move, Root | **Konvergiert** | ADR [0042](adr/0042-m2-divergent-root-note-moves.md); `LocalCore.stageNoteMovePathCollision`; `TestSyncOnceMaterializesDivergentRootNoteMoves`. |
| Move | divergenter Remote-Move innerhalb derselben verifizierten Parent-ID | **Konvergiert** | ADR [0049](adr/0049-m2-divergent-same-parent-note-moves.md); `LocalCore.validateDivergentNestedNoteMove`, `LocalCore.ensureMoveCollisionCanonical`; `TestSyncOnceMaterializesDivergentNestedNoteMoves`. |
| Move | divergenter Remote-Move zwischen verschiedenen Parent-IDs oder Parent mit lokalem Intent | **Fail-closed** | ADR [0049](adr/0049-m2-divergent-same-parent-note-moves.md); `TestSyncOnceRejectsDivergentNestedMoveAcrossParents`, `TestSyncOnceRejectsDivergentNestedMoveWithParentIntent`. Es wird keine Ancestry geraten. |

## Ordner und Struktur

| Lokale Mutation | Remote-Konkurrenz / Serverkonflikt | Status | Nachweis und Grenze |
|---|---|---|---|
| Create/Move/Delete | konfliktfreier Remote-Apply und eigene Echos | **Konvergiert** | ADR [0013](adr/0013-m2-folder-create-apply.md), [0014](adr/0014-m2-folder-move-delete-apply.md), [0030](adr/0030-m2-outbound-folder-intent-binding.md), [0031](adr/0031-m2-cold-history-apply-convergence.md); `LocalCore.ExecuteActiveApplyPlan`; `TestSyncOnceConvergesNestedFolderAndNote`. |
| Create | Pfadkollision oder gelöschter Parent; Folder leer | **Konvergiert** | ADR [0036](adr/0036-m2-empty-folder-create-collision-recovery.md), [0039](adr/0039-m2-folder-parent-unavailable-recovery.md); `LocalCore.recoverEmptyFolderCreateCollision`; `TestSyncOnceRecoversEmptyFolderCreatePathCollision`, `TestSyncOnceRecoversDirectNoteFolderCreateFromUnavailableParent`. |
| Create | direkter Note-Subtree mit nie versuchten linearen Create→Update-Ketten | **Konvergiert** | ADR [0043](adr/0043-m2-direct-note-folder-create-recovery.md), [0050](adr/0050-m2-direct-note-update-chain-folder-recovery.md); `Store.PendingDirectNoteCreates`, `Store.PutConflictFolderCreateRecoveryWithNotes`; `TestSyncOnceRecoversDirectNoteFolderCreatePathCollision`, `TestSyncOnceRecoversUpdatedDirectNoteFolderCreatePathCollision`. |
| Create | rekursiver Subtree mit Nested Foldern, Note-Moves/-Deletes, Branches oder versuchten Operationen | **Fail-closed** | ADR [0043](adr/0043-m2-direct-note-folder-create-recovery.md), [0050](adr/0050-m2-direct-note-update-chain-folder-recovery.md); `TestSyncOnceRejectsUnsupportedNonemptyFolderCreateRecovery`, `TestSyncOnceRejectsNonlinearDirectNoteFolderRecovery`, `TestSyncOnceRejectsAttemptedDirectNoteUpdateFolderRecovery`. |
| Create | dieselbe UUID existiert / `object_exists` | **Fail-closed** | ADR [0041](adr/0041-m2-object-exists-fail-closed.md); `TestSyncOnceFailsClosedForCreateObjectExists`. |
| beliebig | Note/Folder-Typabweichung / `type_mismatch` | **Fail-closed** | ADR [0037](adr/0037-m2-type-mismatch-fail-closed.md); `TestSyncOnceFailsClosedForNoteToFolderTypeMismatch` und `TestSyncOnceFailsClosedForFolderToNoteTypeMismatch` stoppen vor Pull/Apply. |
| Move | `path_collision`, `parent_unavailable` oder `folder_cycle` | **Konvergiert** | ADR [0033](adr/0033-m2-folder-move-conflict-revert.md), [0034](adr/0034-m2-folder-cycle-conflict-revert.md), [0039](adr/0039-m2-folder-parent-unavailable-recovery.md); `LocalCore.revertFolderMoveConflict`; `TestSyncOnceRevertsFolderMovePathCollisionAndKeepsChildEdit`, `TestSyncOnceRevertsFolderMoveFromDeletedParentAndKeepsChildEdit`, `TestSyncOnceRevertsFolderCycleAndKeepsDescendantEdits`. |
| Move | äquivalenter konkurrierender Move / `base_revision_mismatch` | **Konvergiert** | ADR [0035](adr/0035-m2-equivalent-folder-move-resolution.md); `LocalCore.revertFolderMoveConflict`; `TestSyncOnceResolvesEquivalentFolderMoveRevisionConflictAndKeepsChildEdit`. |
| Move | divergenter konkurrierender Move / `base_revision_mismatch` | **Fail-closed** | ADR [0035](adr/0035-m2-equivalent-folder-move-resolution.md); `TestSyncOnceRejectsDivergentFolderMoveRevisionConflict`. Eine allgemeine Zielwahl ist nicht freigegeben. |
| Move | Remote-Delete; lokaler Folder leer | **Konvergiert** | ADR [0045](adr/0045-m2-empty-folder-move-delete-recovery.md); `LocalCore.recoverEmptyFolderMoveAgainstDelete`; `TestSyncOnceRecoversEmptyFolderMoveAgainstRemoteDelete`. |
| Move | Remote-Delete; streng manifestierte direkte Notes mit linearen nie versuchten Updates | **Konvergiert** | ADR [0051](adr/0051-m2-direct-note-folder-move-delete-recovery.md); `Store.PutConflictFolderMoveDeleteRecoveryWithNotes`; `TestSyncOnceRecoversDirectNotesInFolderMoveAgainstRemoteDelete`. |
| Move | Remote-Delete; rekursiver/Nested oder nichtlinearer Subtree | **Fail-closed** | ADR [0051](adr/0051-m2-direct-note-folder-move-delete-recovery.md); `TestSyncOnceRejectsNonemptyFolderMoveAgainstRemoteDelete`, `TestSyncOnceRejectsUnsafeDirectNotesInFolderMoveDeleteRecovery`. |
| Delete | Remote-Folder ist nicht leer / `folder_not_empty` | **Konvergiert** | ADR [0027](adr/0027-m2-folder-not-empty-preservation.md); `LocalCore.restoreFolderNotEmptyConflict`; `TestSyncOnceRestoresFolderRejectedAsNotEmpty`. Remote-Struktur bleibt sichtbar. |
| Delete | Objekt fehlt oder ist bereits tombstoned | **Konvergiert** | ADR [0040](adr/0040-m2-idempotent-stale-deletes.md); `Store.ResolveMissingDelete`; `TestSyncOnceTreatsMissingRemoteFolderDeleteAsSatisfied`. |
| Delete | Remote-Move / `base_revision_mismatch` | **Fail-closed** | Der aktuelle Konfliktsnapshot beweist keinen dauerhaft leeren Subtree. Ein untersuchter Client-only-Ansatz scheiterte an späteren Remote-Kindern beziehungsweise weiteren Moves und blockiertem Pull bei fehlendem lokalem Parent. Eine atomare Serveroperation oder ein revisionsgebundener Subtree-Snapshot sind mögliche Designs, aber noch nicht per ADR festgelegt. |

## Querschnitt, Plattformen und Betrieb

| Bereich | Status | Nachweis und Grenze |
|---|---|---|
| Outbox-Replay, idempotente Submits, Cursor und Crash-Wiederaufnahme | **Konvergiert** | ADR [0011](adr/0011-m2-client-sync-durability.md), [0031](adr/0031-m2-cold-history-apply-convergence.md), [0032](adr/0032-m2-paginated-pull-resumption.md); `LocalCore.SyncOnce`; `TestSyncOnceRetriesAmbiguousAttemptWithSameOperationAndBlobFirst`, `TestExecuteActiveApplyPlanResumesFolderMoveAfterPublicationCrash`, `TestTwoClientsConvergeThroughAuthenticatedHTTP`. |
| Authentifizierte Mehrgeräte-Abnahme | **Konvergiert für die genannten Zellen** | ADR [0029](adr/0029-m2-authenticated-two-client-convergence.md), [0044](adr/0044-m2-authenticated-structural-conflict-convergence.md), [0048](adr/0048-m2-authenticated-post-adr44-convergence.md) und [0052](adr/0052-m2-authenticated-adr49-51-convergence.md); `TestAuthenticatedStructuralConflictsConverge`, `TestAuthenticatedPostADR44Convergence` und `TestAuthenticatedADR49To51Convergence` decken A/B sowie kalten C-Bootstrap über echte Login-, Blob- und Sync-Routen ab. |
| Nicht unterstützter Konflikt neben unbeteiligten Objekten | **Fail-closed; Anforderung offen** | ADR [0054](adr/0054-m2-durable-sync-inbox-foundation.md) ergänzt eine dauerhafte Inbox mit getrenntem Download-/Confirmed-Frontier, ist aber noch nicht in `SyncOnce` verdrahtet. `LocalCore.SyncOnce` gibt bei `Store.HasUnresolvedOutbox` weiterhin vor Pull `ErrUnresolvedOutbound` zurück; [SYNC-012](PRD.md#sync-012--fortsetzbarer-sync) bleibt offen. |
| Blob fehlt oder Hash stimmt nicht | **Fail-closed, technisch getestet** | `TestExecuteActiveApplyPlanRejectsBlobMismatchBeforeFilesystemMutation`, `TestPutLimitsAndHashMismatchLeaveNoPublishedBlob`. Kein stiller Apply/Cursorfortschritt; eine vollständige Nutzer-/Betriebsalarmierung bleibt Produktarbeit. |
| Darwin/Linux Rooted Apply | **Konvergiert im automatisierten Umfang** | Descriptor-relative `*at`-Operationen, `O_NOFOLLOW`, Device/Inode-Prüfung; Repository- und Cross-Build-Tests. Reale Linux-Desktop-Hardware ist extern. |
| Windows Rooted Apply/Staging/Cleanup | **Fail-closed** | `client/internal/repository/rooted_windows.go` verweigert sicherheitskritische Syncpfade, bis Reparse-Point-Schutz handle-basiert implementiert und real getestet ist. |
| Reale Windows-/Linux-Desktoptests | **Extern** | Zielgeräte fehlen; siehe manuelle Testpläne. |
| Docker-Build/-Deployment | **Extern** | Der konfigurierte Remote-Docker-Kontext ist per SSH nicht erreichbar. Container-/Proxy-Abnahme ist kein lokaler Konfliktmatrixbeweis. |

## Abgleich mit den M2-Abnahmekriterien

| PRD-Kriterium | Auditstatus | Begründung |
|---|---|---|
| [M2-AC-001](PRD.md#m2-ac-001) – zwei Offline-Geräte konvergieren ohne stillen Datenverlust | **Teilweise, nicht abgenommen** | Die oben als **Konvergiert** markierten Zellen erfüllen dies automatisiert. Der weiterhin fail-closed behandelte Folder-Delete/Remote-Move und bewusst begrenzte rekursive Strukturzellen verhindern eine pauschale M2-Abnahme. Fail-closed bedeutet keinen stillen Verlust, aber auch keine automatische Konvergenz. |
| [M2-AC-002](PRD.md#m2-ac-002) – definierte Konfliktmatrix besteht Tests | **Teilweise, nicht abgenommen** | Note-Kernmatrix und mehrere Folder-Zellen sind getestet. Die Matrix ist jetzt explizit, enthält aber weiterhin Fail-closed- und protokoll-blockierte Zellen. |
| [M2-AC-003](PRD.md#m2-ac-003) – Requests/Wiederaufnahme erzeugen keine Duplikate | **Erfüllt im automatisierten M2-Core** | Idempotente Serveroperationen, stabile Operations-IDs, Replay-Mismatch-Schutz, persistente Apply-Pläne und Pull-Seiten-Wiederaufnahme sind getestet. |
| [M2-AC-004](PRD.md#m2-ac-004) – fehlender/hashinkonsistenter Blob wird erkannt und alarmiert | **Technischer Core erfüllt; Produktalarmierung offen** | Blob-Repository und Client-Apply lehnen fehlende oder falsche Bytes vor Mutation ab. Sichtbare UI-/Betriebsalarmierung ist noch nicht vollständig. |

Relevante Anforderungen: [SYNC-002](PRD.md#sync-002--später-abgleich), [SYNC-003](PRD.md#sync-003--idempotenz), [SYNC-005](PRD.md#sync-005--tombstones), [SYNC-006](PRD.md#sync-006--bearbeitungskonflikt), [SYNC-008](PRD.md#sync-008--löschen-gegen-bearbeiten-oder-verschieben), [SYNC-009](PRD.md#sync-009--pfadkollision), [SYNC-012](PRD.md#sync-012--fortsetzbarer-sync) und [SYNC-013](PRD.md#sync-013--integritätsfehler).

## Verbleibende automatisierbare Arbeit

Priorisiert, ohne externe Zielhardware:

1. Die Schema-v25-Inbox aus ADR 0054 in Transport, `SyncOnce` und Apply-Scheduling verdrahten und objektbezogene Isolation ungelöster Konflikte implementieren, damit unbeteiligte Änderungen gemäß `SYNC-012` fortschreiten können, ohne den Konfliktzustand zu überspringen.
2. Rekursive Folder-Create- und Folder-Move/Remote-Delete-Recovery für vollständig manifestierte Nested-Folder-DAGs entwerfen und implementieren; Note-Moves/-Deletes sowie attempted/branched Historien zunächst weiter fail-closed lassen.
3. Für divergente Folder Move/Move-Ziele eine explizite Produktregel plus inode-/ancestry-gebundenes Journal entwerfen; bis zur Freigabe bleibt die Zelle fail-closed.
4. Nutzer- und Betriebsalarmierung für `SYNC-013`/M2-AC-004 vervollständigen.
5. Für Folder-Delete/Remote-Move per ADR entscheiden, ob eine atomare Preserve-and-Delete-Serveroperation, ein revisionsgebundener Subtree-Snapshot oder ein anderer beweisbar verlustfreier Ansatz verwendet wird; der bisher untersuchte reine Clientansatz war nicht sicher.

Nicht als lokal automatisierbar gezählt werden reale Windows-/Linux-Gerätetests, Codesigning, Reverse-Proxy-/Deployment-Abnahme und der derzeit nicht erreichbare Remote-Docker-Kontext.
