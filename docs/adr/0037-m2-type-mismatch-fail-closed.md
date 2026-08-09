# ADR 0037: Typkonflikte bleiben fail-closed

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Der Server legt den Objekttyp einer UUID beim Create unveränderlich fest. Ein späteres `type_mismatch` bedeutet deshalb keinen normalen Mehrgerätekonflikt, sondern eine beschädigte oder falsch zugeordnete lokale Identität: Ein Client sendet eine Note-Mutation für eine kanonische Folder-ID oder eine Folder-Mutation für eine kanonische Note-ID.

Eine automatische Auflösung müsste entweder lokale Notizbytes oder einen vollständigen lokalen Folder-Unterbaum auf neue IDs umschreiben. Insbesondere beim Folder wären sämtliche Nachfahren, Baselines und abhängigen Outbox-Operationen transaktional zu rekeyen. Eine teilweise Heuristik könnte Benutzerinhalte verlieren oder zwei unterschiedliche Typen unter derselben UUID vermischen.

## Entscheidung

`type_mismatch` bleibt in M2 ausdrücklich fail-closed:

- Der Server speichert den tatsächlich verwendeten kanonischen Zustand einschließlich Typ, Revision, Parent, Name, Blob und Deleted-Status und liefert ihn bei Replay unverändert zurück.
- Der Client speichert den Konflikt dauerhaft, besitzt aber keinen automatischen Materialisierungszweig für diesen Code.
- Nach dem Submit stoppt `SyncOnce` vor Pull und Remote-Apply mit `ErrUnresolvedOutbound`.
- Lokale Notizbytes, Pfade, Folder-Inodes und Unterbäume werden weder verschoben noch ersetzt.
- Der kanonische Gegentyp wird nicht über den lokalen Inhalt appliziert.
- Wiederholte Sync-Versuche bleiben identisch blockiert.

Automatische Recovery darf später nur als eigenes, crash-resumierbares Rekey-Protokoll ergänzt werden. Für Notes muss es exakte Bytes unter neuer UUID retten; für Folder muss es den vollständigen Unterbaum und alle abhängigen Intents atomar abbilden.

## Verifikation

Adversariale Clienttests decken beide Richtungen ab:

- lokales Note-Update gegen kanonischen Folder,
- lokaler Move eines nichtleeren Folders gegen kanonische Note.

Sie prüfen unveränderte Bytes, Note-ID, Folder-Inode und Kindinhalt sowie den dauerhaften Konflikt und die ausbleibende kanonische Anwendung. Der Servertest prüft kanonischen Typ, Revision, Name und unveränderten Replay.

## Folgen

Ein beschädigtes Gerät konvergiert bei einem Typkonflikt nicht automatisch, verliert aber keine lokale oder kanonische Fassung. Normale Synchronisation bleibt für dieses Gerät blockiert, bis eine explizite Reparatur verfügbar ist. Das ist sicherer als eine unvollständige automatische Strukturumschreibung.
