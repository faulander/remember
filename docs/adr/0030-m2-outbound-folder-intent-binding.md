# ADR 0030: Identitätsgebundene lokale Folder-Move/-Delete-Intents

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein lokaler Folder-Move oder -Delete ist bereits im Dateisystem sichtbar, bevor der Server die Operation akzeptiert. Beim späteren Cursor-Echo fehlt der Quellordner deshalb am alten Pfad. Der Remote-Apply durfte ohne persistierte Device-/Inode-Bindung keinen neuen Folder-Mutationsplan erzeugen und schlug geschlossen fehl. Damit konnten eigene akzeptierte Folder-Moves und -Deletes einen vollständigen Zwei-Geräte-Sync blockieren.

## Entscheidung

Client-Schema v12 ergänzt `sync_outbox_folder_intents`. Beim Reconcile wird für bekannte Folder-Moves und -Deletes zusammen mit der unveränderlichen Outbox-Operation atomar gespeichert:

- Operation- und Folder-ID,
- Mutationstyp,
- ursprünglicher relativer Pfad,
- beobachtete Device-/Inode-Identität.

Die Bindung ist unveränderlich und nicht löschbar. Sie wird niemals an den Server übertragen und verändert die kanonische Operationsform nicht.

Beim Pull darf eine Bindung nur für das exakte akzeptierte Echo derselben Operation, Folder-ID, Mutation, Ergebnisrevision und desselben Ergebniscursors verwendet werden. Zusätzlich muss ein Move im aktuellen Snapshot bereits am kanonischen Ziel mit exakt derselben bekannten Device-/Inode-Identität stehen. Ein Delete muss im Snapshot bereits fehlen. Erst danach entsteht der unveränderliche Apply-Mutationsplan.

Der Ausführungsschritt prüft den realen Ziel-Inode eines lokal bereits ausgeführten Moves erneut. Bei Deletes muss der gebundene Quellpfad weiterhin fehlen. Ersetzungen, wiedererschienene Quellen oder abweichende Inodes schlagen geschlossen fehl.

## Folgen

Eigene akzeptierte Folder-Moves und leere Folder-Deletes können crash-sicher als bereits lokal ausgeführt bestätigt werden, ohne einen Pfad allein als Identitätsbeweis zu verwenden. Ältere oder künstliche Operationen ohne Bindung behalten das bisherige fail-closed-Verhalten. Der authentifizierte Zwei-Client-Test umfasst nun Folder-Create mit Kindnotiz, inferierten Folder-Move, Kind-Delete und abschließenden leeren Folder-Delete.
