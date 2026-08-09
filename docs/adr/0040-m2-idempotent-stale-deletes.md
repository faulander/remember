# ADR 0040: Idempotente stale Deletes gegen kanonische Tombstones

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können dieselbe Note oder denselben leeren Folder offline löschen. Der erste Delete erzeugt den kanonischen Tombstone. Der zweite Delete wird anschließend mit `object_deleted` abgelehnt, obwohl seine Benutzerabsicht bereits erfüllt ist. Ohne explizite Auflösung bleibt der Client unnötig blockiert.

Die bestehende Auflösung `already_deleted` deckte nur `object_missing` ohne kanonischen Zustand ab. Bei `object_deleted` muss zusätzlich bewiesen werden, dass der kanonische Typ zur lokalen Delete-Absicht passt und die Tombstone-Revision wirklich neuer als deren Basis ist.

## Entscheidung

Client-Schema v18 schützt `already_deleted` durch einen SQL-Insert-Guard. Zulässig sind ausschließlich:

- Delete + `object_missing` ohne kanonischen Konfliktzustand oder
- Delete + `object_deleted` mit gleichem Objekttyp, `deleted=true` und kanonischer Revision größer als die lokale Basisrevision.

Der Store wiederholt dieselben Prädikate innerhalb seiner Transaktion. Abhängige pending/attempted Operationen werden wie bisher rekursiv superseded.

Beim späteren Pull darf ein bereits lokal fehlendes Objekt nur dann ohne technische Trash-Datei als angewendet gelten, wenn eine dauerhafte `already_deleted`-Auflösung exakt zu Objekt-ID, Typ und gezogener Tombstone-Revision passt:

- Notes durchlaufen danach weiterhin die normale Abwesenheitsprüfung.
- Folder werden ausschließlich bei bereits fehlendem Snapshot-Objekt als konfliktbedingt deferred markiert.
- Apply-Plan, Baseline und Cursor werden erst durch den normalen transaktionalen Planabschluss bestätigt.

## Verifikation

Mehrgeräte-Tests decken stale Doppel-Deletes für Notes und leere Folder ab. Sie prüfen den dauerhaften Konfliktcode `object_deleted`, die Auflösung `already_deleted`, vollständige Outbox-Auflösung und unveränderte Unterstützung für `object_missing`. Der Security-Review bestätigt die SQL- und Pull-Bindungen.

## Folgen

Gleichzeitige Deletes sind idempotent und blockieren die Synchronisation nicht länger. Ein Typkonflikt, eine nicht neuere Revision oder ein nicht passender Pull-Tombstone kann die No-op-Auflösung nicht verwenden und bleibt fail-closed.
