# Remember-Dokumentation

## Produkt und Architektur

- [`PRD.md`](PRD.md) – verbindliche Produktanforderungen und Abnahmekriterien
- [`DESIGN.md`](DESIGN.md) – technisches Gesamtdesign
- [`M2_CONFLICT_MATRIX.md`](M2_CONFLICT_MATRIX.md) – expliziter Audit der konvergierenden, fail-closed und protokoll-blockierten M2-Zellen
- [`MANUAL_TESTS_M1.md`](MANUAL_TESTS_M1.md) – manuelle Tests des lokalen Datenkerns
- [`MANUAL_TESTS_MAC_CLIENT.md`](MANUAL_TESTS_MAC_CLIENT.md) – abgenommene macOS-Clienttests

## Architecture Decision Records

| ADR | Entscheidung |
|---|---|
| [0001](adr/0001-m1-local-format.md) | Lokales Format und Datenkern |
| [0002](adr/0002-client-ui-stack.md) | Wails, Svelte 5, TypeScript und Vite |
| [0003](adr/0003-m2-identity-core.md) | Interner Identity-Core |
| [0004](adr/0004-local-note-editor.md) | Lokaler Notizeditor |
| [0005](adr/0005-m2-sync-core.md) | Mandantengebundener Sync-Core |
| [0006](adr/0006-m2-blob-repository.md) | Internes Blob-Repository |
| [0007](adr/0007-m2-sessions-devices-core.md) | Sessions- und Devices-Core |
| [0008](adr/0008-m2-auth-http-transport.md) | Begrenzter Auth-HTTP-Transport |
| [0009](adr/0009-m2-blob-http-transport.md) | Authentifizierter Blob-HTTP-Transport und Benutzerquota |
| [0010](adr/0010-m2-sync-http-transport.md) | Authentifizierter Sync-HTTP-Transport |
| [0011](adr/0011-m2-client-sync-durability.md) | Client-Outbox, Blob-Staging und Apply-Plan-Persistenz |
| [0012](adr/0012-m2-client-foreground-sync.md) | Begrenzter Client-HTTP-Transport und Vordergrund-Sync |
| [0013](adr/0013-m2-folder-create-apply.md) | Identitätsgebundener Folder-Create-Apply |
| [0014](adr/0014-m2-folder-move-delete-apply.md) | Inode-gebundener Folder-Move/-Delete-Apply |
| [0015](adr/0015-m2-conflict-canonical-state.md) | Authentifizierter kanonischer Zustand bei Sync-Konflikten |
| [0016](adr/0016-m2-reserved-conflict-namespace.md) | Server-provisionierter reservierter Konfliktbereich |
| [0017](adr/0017-m2-note-update-conflict-materialization.md) | Crash-sichere Materialisierung konkurrierender Notiz-Updates |
| [0018](adr/0018-m2-conflict-staging-cleanup.md) | Identitätsgebundene Bereinigung technischer Konfliktkopien |
| [0019](adr/0019-m2-edit-delete-conflict-materialization.md) | Edit-vs-Delete-Konflikte mit Tombstone-Vorrang |
| [0020](adr/0020-m2-delete-edit-conflict-rebase.md) | Lokaler Delete gegen Remote-Edit mit abhängigem Tombstone-Rebase |
| [0021](adr/0021-m2-note-create-path-collision.md) | Verlustfreie Note-Create-Pfadkollisionen |
| [0022](adr/0022-m2-note-move-path-collision.md) | Note-Move-Pfadkollisionen mit kanonischer Wiederherstellung |
| [0023](adr/0023-m2-note-update-object-missing.md) | Note-Update gegen fehlendes Remote-Objekt |
| [0024](adr/0024-m2-object-missing-move-delete-resolution.md) | `object_missing` für Note-Move und bereits erfüllte Deletes |
| [0025](adr/0025-m2-evacuated-conflict-byte-cleanup.md) | Sichere Bereinigung evakuierter Konfliktbytes |
| [0026](adr/0026-m2-outbox-blob-cleanup.md) | Generationsgebundene Bereinigung finaler Outbox-Blobs |
| [0027](adr/0027-m2-folder-not-empty-preservation.md) | Bewahrung nichtleerer Remote-Ordner gegen lokale Deletes |
| [0028](adr/0028-m2-note-parent-unavailable-rescue.md) | Notizrettung bei nicht verfügbarem Remote-Parent |
| [0029](adr/0029-m2-authenticated-two-client-convergence.md) | Authentifizierte Zwei-Client-Konvergenz im Integrationstest |
| [0030](adr/0030-m2-outbound-folder-intent-binding.md) | Identitätsgebundene lokale Folder-Move/-Delete-Intents |
| [0031](adr/0031-m2-cold-history-apply-convergence.md) | Kalter History-Apply über Serverneustarts |
| [0032](adr/0032-m2-paginated-pull-resumption.md) | Dauerhafte Wiederaufnahme zwischen Pull-Seiten |
| [0033](adr/0033-m2-folder-move-conflict-revert.md) | Identitätsgebundener Revert von Folder-Move-Konflikten |
| [0034](adr/0034-m2-folder-cycle-conflict-revert.md) | Folder-Cycle-Konflikte als identitätsgebundener Move-Revert |
| [0035](adr/0035-m2-equivalent-folder-move-resolution.md) | Äquivalente konkurrierende Folder-Moves |
| [0036](adr/0036-m2-empty-folder-create-collision-recovery.md) | Wiederherstellung leerer Folder-Create-Pfadkollisionen |
| [0037](adr/0037-m2-type-mismatch-fail-closed.md) | Fail-closed-Behandlung von Typkonflikten |
| [0038](adr/0038-m2-note-move-delete-convergence.md) | Konvergenz konkurrierender Note-Moves und -Deletes |
| [0039](adr/0039-m2-folder-parent-unavailable-recovery.md) | Folder-Recovery bei fehlendem Parent |
| [0040](adr/0040-m2-idempotent-stale-deletes.md) | Idempotente stale Deletes gegen Tombstones |
| [0041](adr/0041-m2-object-exists-fail-closed.md) | Fail-closed-Behandlung wiederverwendeter Create-UUIDs |
| [0042](adr/0042-m2-divergent-root-note-moves.md) | Divergente konkurrierende Root-Note-Moves |
| [0043](adr/0043-m2-direct-note-folder-create-recovery.md) | Recovery nichtleerer Folder-Creates mit direkten Notes |
| [0044](adr/0044-m2-authenticated-structural-conflict-convergence.md) | Authentifizierte Strukturkonflikt-Konvergenz |
| [0045](adr/0045-m2-empty-folder-move-delete-recovery.md) | Leerer Folder-Move gegen Remote-Delete |
| [0046](adr/0046-m2-equivalent-root-note-moves.md) | Äquivalente konkurrierende Root-Note-Moves |
| [0047](adr/0047-m2-equivalent-nonroot-note-moves.md) | Äquivalente konkurrierende Nicht-Root-Note-Moves |
| [0048](adr/0048-m2-authenticated-post-adr44-convergence.md) | Authentifizierte Konvergenz der Strukturzellen nach ADR 0044 |
| [0049](adr/0049-m2-divergent-same-parent-note-moves.md) | Divergente Nicht-Root-Note-Moves innerhalb derselben Parent-ID |
| [0050](adr/0050-m2-direct-note-update-chain-folder-recovery.md) | Folder-Create-Recovery mit linearen direkten Note-Updates |
| [0051](adr/0051-m2-direct-note-folder-move-delete-recovery.md) | Folder-Move/Remote-Delete-Recovery mit direkten Note-Updates |
| [0052](adr/0052-m2-authenticated-adr49-51-convergence.md) | Authentifizierte Konvergenz für ADR 0049 bis 0051 |
| [0053](adr/0053-m2-descriptor-recursive-subtree-verification.md) | Descriptor-rekursive exakte Subtree-Verifikation |
| [0054](adr/0054-m2-durable-sync-inbox-foundation.md) | Dauerhafte Sync-Inbox-Grundlage mit getrenntem Download-/Confirmed-Fortschritt |
| [0055](adr/0055-m2-sync-inbox-mirror.md) | Sync-Inbox als crash-fortsetzbarer Spiegel des cursor-geordneten Apply-Pfads; Isolation bleibt offen |
| [0056](adr/0056-m2-inbox-linked-out-of-order-apply-plans.md) | Inbox-gebundene Einzelpläne für out-of-order Root-Note-Apply ohne Scheduler |
| [0057](adr/0057-m2-root-note-out-of-order-scheduler.md) | Begrenzter out-of-order Scheduler für unabhängige Root-Note-Updates/-Deletes |
| [0058](adr/0058-m2-authenticated-root-note-isolation.md) | Authentifizierte A/B-/Cold-C-Abnahme der Root-Note-Isolation hinter divergentem Folder-Move |
| [0059](adr/0059-m2-inbox-plan-retry.md) | Expliziter Retry abgebrochener unveränderlicher Inbox-Pläne |
| [0060](adr/0060-m2-ordered-independent-note-chains.md) | Geordnete unabhängige Root-Note-Revisionsketten im begrenzten Scheduler |
| [0061](adr/0061-m2-starvation-free-root-note-selection.md) | Starvation-freie Root-Note-Auswahl vor der Kandidatenbegrenzung |

ADRs dokumentieren einen zum jeweiligen Zeitpunkt akzeptierten Schnitt. Spätere Entscheidungen dürfen frühere ADRs ergänzen, müssen Abweichungen aber ausdrücklich benennen.
