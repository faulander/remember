# ADR 0034: Folder-Cycle-Konflikte als identitätsgebundener Move-Revert

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein lokal gültiger Folder-Move kann serverseitig einen Zyklus erzeugen, wenn ein anderes Gerät den Ziel-Parent inzwischen unter den zu verschiebenden Folder bewegt hat. Lokal kennt der stale Client diese neue Ancestry noch nicht; der Dateisystem-Move ist deshalb ausführbar. Der Server lehnt ihn nach Prüfung des aktuellen Graphen mit `folder_cycle` ab.

## Entscheidung

`folder_cycle` verwendet denselben v13-Move-Revert wie `path_collision` und `parent_unavailable`. Der kanonische Snapshot muss weiterhin einen nicht gelöschten Folder auf exakt der Move-Basisrevision beschreiben. Quellpfad, Device/Inode, fehlende spätere Same-Folder-Operationen, monotones Journal und SQL-geschützte Auflösung bleiben unverändert.

Der stale lokale Folder wird vor dem Pull der neuen Remote-Ancestry an seinen kanonischen Pfad zurückverschoben. Anschließend kann der Remote-Move des ehemaligen Ziel-Parents normal angewendet werden. Nur exakt durch diesen verifizierten Ancestor-Revert übersetzte Nachfahrenpfade werden beim Reconcile unterdrückt. Abhängige Änderungen an Kindobjekten behalten ihre eigenen gültigen Basisrevisionen und dürfen nach `folder_move_reverted` weiterlaufen.

Migration 014 ersetzt ausschließlich den Insert-Guard der Konfliktauflösung und erlaubt `folder_cycle` nur bei einem abgeschlossenen passenden Revert-Journal. Migration 013 bleibt checksum-stabil.

## Folgen

Divergente Folder-Ancestry konvergiert ohne lokale Kindinhalte zu verlieren und ohne einen zyklischen Servergraphen zuzulassen. Der automatisierte Test verschiebt remote `X` unter `F`, bewegt auf einem stale Client `F` unter `X`, bearbeitet anschließend ein Kind und weist nach, dass beide Teilbäume sowie das Edit nach Revert und Pull konvergieren.
