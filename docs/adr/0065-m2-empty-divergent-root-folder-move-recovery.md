# ADR 0065: Leere divergente Root-Folder-Moves

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

ADR 0064 legt fest, dass bei divergentem Folder Move/Move der kanonische Serverpfad die ursprüngliche Folder-UUID behält und die lokale Verliererfassung unter `_Konflikte/Wiederhergestellt` bewahrt wird.

## Entscheidung

Client-Schema v30 implementiert ausschließlich leere root-level Folder mit root-level kanonischem Ziel. Das monotone Journal `conflict_folder_divergent_move_recoveries` bindet Konfliktoperation, Original-/Recovery-UUID, Replacement-Create, lokale und kanonische Pfade, Source-Device/Inode, kanonische Revision sowie Publikationsnonce und -inode exakt.

Die Zustände lauten `prepared → evacuated → canonical_prepared → canonical_published → completed`:

- Vor `evacuated` wird der exakte leere Inode bewegt und trusted reconciled. Jeder Fehler nach Move, nach Reconcile oder beim SQL-Übergang verschiebt denselben Inode zurück und bindet die Original-ID wieder an den lokalen Ausgangspfad.
- `canonical_prepared` persistiert Nonce, Device und Inode der Stage **vor** dem exklusiven Publish. Replay akzeptiert nur exakt dieselbe Publication entweder an der Stage oder bereits am kanonischen Ziel.
- Erst `canonical_published` entfernt den Nonce-Marker und bindet die Original-ID per trusted Reconcile an den kanonischen Root-Pfad.
- Completion verlangt eine Baseline, deren `operation_id` auf eine immutable angewendete Inbox-Folder-Move-Zeile derselben UUID, Revision, root Parent-ID, desselben kanonischen Namens und `deleted=false` zeigt. Spätere oder abhängige aktive Outbox-Operationen werden beim Evacuated-Übergang und bei Completion erneut ausgeschlossen.
- Danach wird exakt ein Folder-Create mit neuer Recovery-UUID unter `ConflictRecoveredID` eingereiht.

Nichtleere, nested, different-parent, belegte Ziele und unbekannte Inodes bleiben ohne Dateisystemmutation `ErrUnresolvedOutbound`. Windows bleibt durch die rooted Repository-Primitive fail-closed.

## Verifikation

Tests prüfen A/B-Konvergenz, neue Recovery-UUID, erhaltenen Verlierer-Inode, Restart, Replacement-Create und SQL-Immutabilität/No-delete/No-replace. Fault-Injection deckt Move, Reconcile, Evacuated-Transition, Stage-Create, Publish-vor-State und Cleanup-vor-Completion ab. Ein nichtleerer Root-Subtree und bestehende nested/different-parent Fälle bleiben unverändert fail-closed.

## Folgen

Die erste ADR-0064-Zelle konvergiert verlustfrei und crash-fortsetzbar. Direkte Notes und rekursive Subtrees sind ausdrücklich nicht freigegeben.
