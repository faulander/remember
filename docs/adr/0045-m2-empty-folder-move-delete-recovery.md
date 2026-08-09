# ADR 0045: Leerer Folder-Move gegen Remote-Delete

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann einen leeren Folder offline verschieben, während ein anderes Gerät dieselbe Folder-ID löscht. Der Server akzeptiert den Delete nur für einen leeren Folder und lehnt anschließend den stale Move mit `object_deleted` ab. Auch der lokal gewählte neue Ort ist sichtbare Benutzerstruktur und darf nicht still verloren gehen.

## Entscheidung

Client-Schema v20 ergänzt das monotone Journal `conflict_folder_move_delete_recoveries` und die Auflösung `folder_move_deleted_recovered`.

Automatische Recovery gilt ausschließlich für:

- Folder-Move mit authentifiziertem kanonischem Folder-Tombstone,
- strikt höhere kanonische Revision und keinen Blob,
- bekannte, bereits beim Move-Outbox-Intent gebundene Device-/Inode-Identität,
- descriptor-verifizierte Leere,
- keine spätere aktive Operation derselben Folder-ID und keine aktiven Abhängigkeiten.

Der exakte lokal verschobene Inode wird crash-resumierbar unter neuer UUID direkt nach `_Konflikte/Wiederhergestellt` bewegt. Trusted Reconcile entfernt die tombstoned Original-ID und bindet ausschließlich denselben Inode an die persistierte Recovery-ID. Im Zustand `moved` darf der Client den Remote-Tombstone pullen. Ein bereits fehlendes Original gilt beim Folder-Delete-Apply nur dann als lokal angewendet, wenn Journal, Outbox, kanonischer Typ und die exakte gezogene Tombstone-Revision übereinstimmen.

Erst wenn sowohl die Originalprojektion auf der kanonischen Tombstone-Revision als auch `ConflictRecoveredID` dauerhaft bestätigt sind, werden neuer Folder-Create und Konfliktauflösung atomar gespeichert. Die Original-ID bleibt tombstoned.

Ein nach dem Move nicht mehr leeres Verzeichnis, spätere Same-Folder-Operationen oder abhängige Intents bleiben ohne Dateisystemmutation fail-closed. Windows bleibt mangels handle-sicherer Rooted-Move-Unterstützung fail-closed.

## Verifikation

Tests prüfen physischen Move, Crash nach Trusted Reconcile, Neustart, neue UUID, erhaltene Device-/Inode-Identität, Server-Tombstone, synchronisierten Recovery-Folder, zweites Gerät und kaltes Drittgerät. Negative Tests prüfen nichtleere Folder und eine später eingefügte Same-Folder-Operation.

## Folgen

Die Richtung lokaler leerer Move gegen Remote-Delete konvergiert verlustfrei. Die umgekehrte Richtung lokaler Delete gegen Remote-Move bleibt separat: Dort existiert lokal kein Inode mehr und die kanonische Move-Struktur muss zunächst authentifiziert materialisiert oder als neuer Recovery-Folder publiziert werden.
