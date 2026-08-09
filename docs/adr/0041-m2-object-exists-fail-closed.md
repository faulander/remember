# ADR 0041: Create-`object_exists` bleibt fail-closed

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Der Server weist einem neuen Objekt seine UUID beim ersten akzeptierten Create dauerhaft zu. Ein späterer Create mit derselben UUID erhält `object_exists`, unabhängig davon, ob Typ, Parent, Name oder Inhalt übereinstimmen. Bei UUIDv7 ist eine natürliche Kollision praktisch ausgeschlossen; der Zustand weist deshalb auf einen beschädigten beziehungsweise falsch wiederhergestellten lokalen Index oder eine fehlerhaft wiederverwendete Identität hin.

Eine automatische Behandlung darf weder das kanonische Serverobjekt überschreiben noch lokale Notizbytes oder einen lokalen Folder-Unterbaum derselben ID verwerfen. Besonders bei Foldern wäre für eine sichere Rettung ein vollständiges Subtree-Rekeying erforderlich.

## Entscheidung

Create-`object_exists` bleibt ausdrücklich fail-closed:

- Der Server persistiert den tatsächlich verwendeten kanonischen Zustand und liefert ihn beim Replay unverändert.
- Der Client speichert den Konflikt dauerhaft, besitzt aber keinen automatischen `object_exists`-Zweig.
- `SyncOnce` stoppt nach dem Submit vor Pull und Apply mit `ErrUnresolvedOutbound`.
- Lokale Note-Bytes, Folder-Inodes und Unterbäume bleiben unverändert.
- Das kanonische Serverobjekt wird nicht überschrieben.
- Wiederholte Sync-Versuche bleiben identisch blockiert.

Ein vorheriger content-addressed Blob-Upload und die serverseitige Konflikt-Namespace-Provisionierung verändern weder das kollidierende kanonische Objekt noch den lokalen Baum.

Eine spätere automatische Reparatur muss die lokale Fassung unter neuer UUID retten. Für nichtleere Folder gilt dabei dasselbe transaktionale Subtree-Rekeying wie bei nichtleeren Folder-Create-Pfad- und Parent-Konflikten.

## Verifikation

Adversariale Tests spiegeln die reale Serverreihenfolge wider und decken ab:

- lokale Note gegen kanonischen Folder derselben UUID,
- lokalen nichtleeren Folder-Unterbaum gegen kanonischen Folder derselben UUID.

Sie prüfen exakte Bytes, Folder-Inode, Kindinhalt, unveränderten kanonischen Zustand, dauerhaften Konflikt und wiederholtes Blocking.

## Folgen

Eine beschädigte Identitätszuordnung konvergiert nicht automatisch, verliert aber weder lokale noch kanonische Inhalte. Die normale Mehrgeräte-Konfliktmatrix wird nicht durch eine unsichere UUID-Heuristik erweitert.
