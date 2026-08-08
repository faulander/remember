# ADR 0027: Bewahrung nichtleerer Remote-Ordner gegen lokale Deletes

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Client kann einen lokal leeren Ordner löschen, während ein anderes Gerät bereits neue Kinder unter derselben Folder-UUID synchronisiert hat. Der Server lehnt den Delete nach erfolgreicher Revisionsprüfung mit `folder_not_empty` ab. Da der lokale Ordner physisch fehlt, können die anschließend gepullten Kinder ohne sichere Wiederherstellung ihrer Parent-Identität nicht angewendet werden.

## Entscheidung

Schema v11 ergänzt ein unveränderliches, operationsgebundenes `conflict_folder_restorations`-Journal. Es persistiert Folder-ID, kanonischen Zielpfad, technischen Stagepfad, 256-Bit-Nonce sowie Device/Inode und erzwingt ausschließlich `prepared → published → completed`. Zeilen dürfen nicht gelöscht werden; dieselbe Folder-ID kann in späteren unabhängigen Konflikten erneut restauriert werden.

Unterstützt wird ausschließlich ein Folder-Delete mit `folder_not_empty`, vollständigem nicht gelöschtem kanonischem Folder-Zustand und einer kanonischen Revision, die exakt der Delete-Basis entspricht. Der Parent muss lokal als bekannte, inode-gebundene Folder-Identität vorliegen. Der fehlende Ordner wird exklusiv aus einem privaten Nonce-Stage veröffentlicht. Parent- und Ziel-Inodes werden vor, während und nach einem trusted Reconcile geprüft; fremde oder bereits vorhandene Zielordner werden nicht übernommen.

Nach erfolgreichem Reconcile wird der Nonce-Marker descriptor-relativ bereinigt. Journalabschluss und die unveränderliche Konfliktauflösung `folder_not_empty_preserved` erfolgen atomar; abhängige pending/attempted Operationen werden rekursiv superseded. Erst danach gilt die Outbox als aufgelöst und der normale Pull kann die Remote-Kinder anwenden.

Ein Absturz nach Ordnerpublikation, vor Reconcile, nach Marker-Cleanup oder vor SQLite-Abschluss nimmt dieselbe Nonce-/Inode-Bindung wieder auf. Der ursprüngliche Delete bleibt als konfliktbehaftete Outbox-Historie erhalten.

## Folgen

Remote-Kinder gewinnen gegen das Löschen eines nur lokal leer beobachteten Ordners. Der Client rekonstruiert keine Inhalte und rät keine Folder-Identität, sondern stellt ausschließlich den authentifizierten Parent-Knoten sicher wieder her und zieht anschließend dessen echte Kinder. Folder-Move-/Create-Kollisionen und andere Strukturkonflikte bleiben separat.
