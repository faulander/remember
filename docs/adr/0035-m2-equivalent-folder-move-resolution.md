# ADR 0035: Äquivalente konkurrierende Folder-Moves

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei stale Geräte können denselben Folder unabhängig zum selben logischen Parent und Namen verschieben. Die erste Operation erhöht die Serverrevision; die zweite wird trotz identischem Endzustand mit `base_revision_mismatch` abgelehnt. Lokale Kindänderungen können von der zweiten Move-Operation abhängen und sollen weiterlaufen.

Ein allgemeiner Revert divergenter Move-Ziele ist mit dem heutigen kanonischen Konfliktsnapshot nicht sicher: Er enthält Parent-ID und Namen des Folders, aber nicht die authentifizierten Pfade der gesamten Parent-Ancestry. Das Auflösen eines remote anders verschobenen Parents anhand eines stale lokalen Snapshots könnte einen Zielpfad innerhalb des eigenen Unterbaums konstruieren.

## Entscheidung

Migration 015 erweitert den SQL-Guard für `folder_move_reverted` um `base_revision_mismatch`. Die automatische Auflösung ist jedoch ausschließlich für äquivalente Moves zulässig:

- kanonische Revision ist strikt größer als die lokale Basis,
- kanonische Parent-ID entspricht exakt der vorgeschlagenen Parent-ID,
- kanonischer Name entspricht exakt dem vorgeschlagenen Namen,
- der lokale Folder steht bereits am daraus lokal beobachteten Ziel mit dem gebundenen Device/Inode,
- alle übrigen Move-Revert-Invarianten bleiben erfüllt.

Da versuchter und kanonischer logischer Zielzustand identisch sind, erfolgt kein Dateisystem-Move und kein pfadübersetzendes Reconcile. Das Journal verifiziert lediglich den vorhandenen Inode, schließt als `folder_move_reverted` ab und gibt abhängige Kindoperationen frei. Der nachfolgende Pull übernimmt die authentifizierte Serverrevision und spätere Ancestor-Änderungen.

Unterschiedliche Parent-IDs oder Namen schlagen geschlossen fehl. Der Client rät keinen Remote-Parent-Pfad und verändert den lokalen Unterbaum nicht.

## Folgen

Identische konkurrierende Folder-Moves konvergieren ohne künstlichen Konfliktordner; abhängige Kindedits bleiben erhalten. Echte Move/Move-Konflikte mit unterschiedlichen Zielen bleiben offen, bis das Protokoll einen authentifizierten Ancestor-Pfad oder eine andere strukturtreue Materialisierung bereitstellt.
