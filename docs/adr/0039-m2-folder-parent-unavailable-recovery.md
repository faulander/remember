# ADR 0039: Folder-Create- und Folder-Move-Recovery bei fehlendem Parent

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann unter einem lokal noch sichtbaren Folder einen neuen Folder erzeugen oder einen bestehenden Folder dorthin verschieben, nachdem ein anderes Gerät den Zielparent bereits gelöscht hat. Der Server lehnt beide Operationen mit `parent_unavailable` ab.

Folder-Moves besitzen bereits einen authentifizierten kanonischen Zustand und eine vor dem Outbox-Enqueue gebundene Inode-Identität. Folder-Creates besitzen dagegen keinen kanonischen Zustand für ihre neue UUID. Auch ein leerer neuer Folder ist Benutzerstruktur und darf nicht still verschwinden.

## Entscheidung

Client-Schema v17 erweitert die bestehende Empty-Folder-Create-Recovery checksum-stabil auf `parent_unavailable`. Migration 017 ersetzt ausschließlich die beiden SQL-Guards für Journal-Insert und Auflösung; Migration 016 bleibt unverändert.

### Leerer Folder-Create

Für einen Create ohne kanonischen Zustand gelten dieselben strikten Voraussetzungen wie bei einer Pfadkollision:

- bekannte Device-/Inode-Identität,
- descriptor-verifizierte Leere,
- keine indexierten Nachfahren,
- keine späteren aktiven Operationen derselben ID und keine aktiven Abhängigkeiten.

Der exakte Inode wird unter neuer UUID direkt nach `_Konflikte/Wiederhergestellt` verschoben. Trusted Reconcile entfernt die alte, nie serverseitig angelegte ID über eine exakte Quellpfad-/ID-Bindung und ordnet denselben Inode ausschließlich der persistierten Recovery-ID zu. Erst nach dauerhafter Baseline des reservierten Parents wird der neue Folder-Create atomar eingeordnet und die ursprüngliche Operation aufgelöst. Der Remote-Delete des alten Parents kann danach ohne scheinbares lokales Kind angewendet werden.

Nichtleere neue Folder bleiben fail-closed, bis ein vollständiges Subtree-Rekeying verfügbar ist.

### Folder-Move

Ein Folder-Move in einen gelöschten Parent verwendet weiterhin das Inode-gebundene Move-Revert-Journal. Der exakte Folder kehrt an seinen authentifizierten kanonischen Pfad zurück. Deterministisch mitbewegte Nachfahren und ihre lokalen Edits bleiben erhalten und sendbar; anschließend wird der gelöschte Zielparent gepullt.

## Verifikation

Tests prüfen:

- Empty-Create-Recovery aus einem gelöschten Parent und Konvergenz zum zweiten Gerät,
- unveränderte bestehende Path-Collision-Recovery,
- Folder-Move-Revert nach Parent-Delete,
- Absturz nach physischem Revert,
- Erhalt und anschließende Synchronisation eines abhängigen Kind-Edits.

## Folgen

Leere Folder-Struktur geht bei einem verschwundenen Zielparent nicht mehr verloren. Die noch offene nichtleere Create-Variante bleibt klar fail-closed und wird gemeinsam mit dem allgemeinen Subtree-Rekeying behandelt.
