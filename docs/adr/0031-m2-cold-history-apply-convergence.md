# ADR 0031: Kalter History-Apply über Serverneustarts

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein neu angemeldetes Gerät muss die vollständige Cursor-Historie anwenden können, auch wenn mehrere Änderungen desselben Objekts oder einer Hierarchie in einer Pull-Seite liegen. Ein produktionsnaher Test nach Serverneustart deckte drei Lücken auf:

1. Der virtuelle Pfad einer früher in derselben Seite erzeugten Notiz folgte einem späteren Move ihres Parent-Ordners nicht.
2. Reconcile zwischen Apply-Schritten konnte bereits angewendete Remote-Zwischenzustände irrtümlich als lokale Outbox-Intents erfassen.
3. Ein Folder, der innerhalb derselben Seite erstellt und wieder gelöscht wurde, enthielt beim `rmdir` noch den technischen Publikationsmarker und war nach einem Crash nicht resumierbar.

## Entscheidung

Der Apply-Preflight führt virtuelle Notizpfade bei einem Remote-Folder-Move auf den vom authentifizierten Plan vorgegebenen Zielpräfix nach. Dadurch können spätere Note-Moves/-Deletes derselben Seite ihren tatsächlichen Zwischenpfad exakt prüfen.

Reconcile bleibt während des Apply vollständig aktiv und darf unabhängige lokale Änderungen weiterhin erfassen. Unterdrückt werden ausschließlich exakt erwartete Remote-Zustände:

- Notiz-ID, SHA-256 und erwarteter relativer Pfad,
- Folder-ID und erwarteter relativer Pfad,
- bei einem inode-gebundenen Ancestor-Move die daraus deterministisch übersetzten Pfade seiner beobachteten Nachfahren.

Eine externe zweite Verschiebung oder Inhaltsänderung stimmt nicht mit diesem Zustand überein und wird als lokaler Intent erfasst beziehungsweise lässt Apply geschlossen fehlschlagen.

Bei Folder-Create gefolgt von Folder-Delete im selben aktiven Plan wird der 256-Bit-Nonce-Marker nur am gebundenen Device/Inode entfernt. Das Journal markiert die Publikation atomar als durch den passenden Delete konsumiert. Nach einem Crash darf der frühere Create-Schritt dann die inzwischen absente Folder-Inode überspringen; Marker-Cleanup und descriptor-relatives `rmdir` bleiben idempotent resumierbar.

Der HTTP-Integrationstest startet den Server auf denselben SQLite-/Blob-Pfaden neu, verwendet bestehende Access-Tokens weiter und bootstrapped anschließend ein drittes leeres Gerät aus der vollständigen Historie. Danach prüft er Note-Move/-Delete sowie Folder-Create/-Move/-Delete und die finalen Tombstones.

## Folgen

Mehrere Zustände derselben Note-/Folder-Hierarchie können in einer Pull-Seite konvergieren. Remote-Apply absorbiert keine unabhängigen lokalen Edits. Der Test belegt zusätzlich die Persistenz von Sitzungen, Sync-Historie und Blobs über einen echten Serverneustart; reale Plattform- und Prozess-Crash-Tests bleiben separat.
