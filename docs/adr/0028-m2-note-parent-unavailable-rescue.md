# ADR 0028: Notizrettung bei nicht verfügbarem Remote-Parent

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann eine Notiz in einem Ordner erstellen oder dorthin verschieben, während ein anderes Gerät diesen Parent bereits gelöscht hat. Der Server lehnt Create oder Move mit `parent_unavailable` ab. Der lokale Zielpfad kann anschließend durch den Pull des Parent-Tombstones verschwinden; lokale Notizbytes und davon abhängige Edits dürfen dabei nicht verloren gehen.

## Entscheidung

Für Note-Create mit `parent_unavailable` und `canonical: null` wird der bestehende Canonical-absent-Rettungspfad verwendet. Die lokalen Bytes werden hashgebunden gestaged, crash-sicher in technischen Trash evakuiert und mit neuer UUIDv7 sowie Herkunftsmetadaten unter `_Konflikte/Wiederhergestellt` materialisiert. Erst dann darf der Remote-Tombstone den alten Parent entfernen.

Für Note-Move muss der kanonische Snapshot eine nicht gelöschte Notiz mit exakt validiertem Blob-Hash enthalten. Die Fassung am fehlgeschlagenen Ziel wird ebenfalls gestaged und evakuiert. Anschließend wird die kanonische Serverfassung aus dem authentifizierten Blob an ihrem kanonischen Parent/Pfad wiederhergestellt und gegen Revision, Parent, Name, Hash und Frontmatter-ID geprüft. Abhängige lokale Operationen werden rekursiv superseded, nachdem ihre Bytes in der sichtbaren Konfliktkopie enthalten sind.

`parent_unavailable` zählt während `copy_staged` und `copy_published` als aktive Evakuierung. Nach einem Absturz überspringt `Open` deshalb allgemeines Reconcile, bis die selektive Delete-Unterdrückung und die Rettung wieder aufgenommen wurden. Technische Evakuierungsbytes werden erst nach dauerhaft indexierter und synchronisierbarer Konfliktkopie descriptor-gebunden bereinigt.

## Folgen

Der Server-Tombstone des Parents bleibt wirksam, ohne lokale Note-Create- oder Note-Move-Inhalte zu verlieren. Folder-Create/-Move-Konflikte benötigen weiterhin eigene, strukturwahrende Rebase-Regeln.
