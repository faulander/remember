# ADR 0022: Note-Move-Pfadkollisionen mit kanonischer Wiederherstellung

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Client kann eine Notiz – gegebenenfalls zusammen mit einem abhängigen lokalen Edit – auf einen Pfad verschieben, den ein anderes Gerät inzwischen belegt hat. Der Server lehnt den Move mit `path_collision` ab und liefert den kanonischen Zustand der verschobenen Quellnotiz. Der Client muss den Remote-Gewinner am Zielpfad, den kanonischen Zustand der Quellnotiz und die lokal verschobene Fassung gleichzeitig erhalten.

## Entscheidung

Bei einem Note-Move-`path_collision` werden die exakten lokalen Bytes einschließlich abhängiger Edits mit neuer UUID und Konfliktherkunft technisch gestaged. Die versuchte Zieldatei wird über denselben crash-resumierbaren, hashgebundenen Evakuierungsweg wie in ADR 0021 entfernt; Reconcile unterdrückt dabei eine falsche lokale Delete-Absicht für die Quell-UUID.

Der authentifizierte kanonische Blob aus dem Konfliktsnapshot wird an dessen kanonischem Parent/Name wiederhergestellt und über Note-ID und SHA-256 geprüft. Fehlt der kanonische Parent noch lokal, wartet die Wiederherstellung auf dessen Pull-Apply. Zwischenliegende Remote-Updates und -Moves der Quellnotiz werden bis zur gespeicherten Konfliktrevision im Apply-Journal verarbeitet, aber nicht über die bereits wiederhergestellten finalen Bytes veröffentlicht.

Erreicht der Pull exakt die kanonische Konfliktrevision, müssen Parent, Name, Blob-Hash und Tombstone-Zustand vollständig dem unveränderlichen Konfliktsnapshot entsprechen. Erst danach darf diese Revision zur Baseline werden. Die sichtbare Konfliktkopie wird zusätzlich erst nach kanonischer Projektion der Quellnotiz und dauerhafter Bereitstellung des reservierten Wiederherstellungsordners veröffentlicht.

## Folgen

Der akzeptierte Remote-Pfadinhaber bleibt am versuchten Ziel, die ursprüngliche Quell-UUID konvergiert auf ihren authentifizierten kanonischen Pfad und die lokal verschobene beziehungsweise bearbeitete Fassung bleibt als neue synchronisierte Konfliktnotiz sichtbar. Folder-Moves und weitere Strukturkollisionen bleiben getrennte Konfliktklassen.
