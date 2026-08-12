# ADR 0073: Authentifizierte leere Folder Delete/Remote-Move-Konvergenz

- Status: Angenommen
- Datum: 2026-08-12

`SyncOnce` fordert die ADR-0070-Resolution ausschließlich für einen exakt gespeicherten lokalen Folder-Delete-`base_revision_mismatch` mit aktiver kanonischer Folder-Revision an. Schema v32 bewahrt die Resolution-ID über Retry; erfolgreicher HTTP-Abschluss bindet Recovery-ID und Cursor.

Beim normalen Pull behandelt der Client den zwischenzeitlichen kanonischen Move und den abschließenden Tombstone der bereits lokal gelöschten Original-UUID ausschließlich dann als lokal erfüllt, wenn die exakte aufgelöste Resolution passt. Der Server akzeptiert den leeren Slice nur, wenn der Folder in seiner unveränderlichen Versionshistorie niemals Kinder besaß; damit kann kein intervenierender Child-Verlauf den absent-local Apply blockieren. Recovery-Create, kanonischer Move und Tombstone werden gegen gespeicherte Parent-/Name-/Revision-/Cursor-Deskriptoren geprüft. Der serverseitige Recovery-Create wird über den bestehenden descriptor-sicheren Folder-Publication-Pfad angewendet. Der ursprüngliche Konflikt bleibt nach `resolved` nicht mehr als ungelöster Intent oder erneut zu stageender Konflikt aktiv.

`TestAuthenticatedEmptyFolderDeleteAgainstRemoteMoveConverges` prüft echte A/B-Logins: A löscht offline, B verschiebt remote, A löst atomar auf, Recovery-Create liegt vor Tombstone. Eine simuliert verlorene HTTP-Antwort nach Server-Commit wird nach Client-Neustart mit derselben persistenten Resolution-ID replayt und konvergiert ohne Duplikat. A, B und kalter C sehen nur den neuen Recovery-Folder unter `_Konflikte/Wiederhergestellt`; alte Pfade und Original-UUID sind tombstoned.

Nichtleere Folder bleiben im ADR-0070-Core fail-closed. Die volle rekursive ADR-0069-Zelle bleibt offen.
