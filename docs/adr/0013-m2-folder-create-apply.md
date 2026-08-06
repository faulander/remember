# ADR 0013: Identitätsgebundener Folder-Create-Apply

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Ordner tragen bewusst keine permanente Identitätsdatei. Ein nach einem Absturz bereits vorhandener Zielordner darf daher nicht allein anhand seines Pfades mit einer Server-UUID verbunden werden. Andernfalls könnte ein fremder oder konkurrierend ausgetauschter Ordner nachträglich als Remote-Objekt übernommen werden.

## Entscheidung

Schema v3 ergänzt `apply_folder_publications`. Für jeden Folder-Create-Schritt werden Plan, Schritt, Folder-ID, Zielpfad, deterministischer technischer Stage-Pfad, ein zufälliger 256-Bit-Nonce sowie Geräte-/Inode-Identität unveränderlich gespeichert. Nur `cleanup_authorized` und anschließend der unveränderliche Cleanup-Zeitpunkt dürfen monoton gesetzt werden.

Darwin/Linux erstellen den noch leeren Ordner zunächst descriptor-verankert unter `.remember/apply/folders/<plan>/<step>`. Ein fsync-gesicherter Marker mit Modus `0600` trägt den Nonce. Erst nach Persistenz der Inode-/Nonce-Bindung wird der Stage-Ordner per exklusivem No-Replace-Rename veröffentlicht. Vor und nach Reconcile werden Ziel-Inode und Nonce geprüft. Reconcile erhält die Remote-Folder-ID ausschließlich nach dieser Prüfung und unterdrückt nur den exakt gebundenen Folder-Create-Echo.

Das Markieren des Apply-Schritts und die Cleanup-Autorisierung erfolgen in einer SQLite-Transaktion. Der temporäre Marker bleibt bis zum atomaren Abschluss des gesamten Apply-Plans erhalten und wird erst danach entfernt; ein persistierter Cleanup-Zeitpunkt macht diese Bereinigung nach einem Neustart wiederholbar. Ein Absturz vor der DB-Bindung hinterlässt nur einen technischen Orphan-Stage, der kontrolliert entfernt und neu erstellt werden darf; ein Absturz nach Veröffentlichung erkennt ausschließlich die persistierte Inode-/Nonce-Kombination. Ein bereits vorhandener, ungebundener Zielordner schlägt geschlossen fehl. Windows bleibt bis zu einer handle-sicheren Reparse-Point-Implementierung geschlossen.

Geordnete, verschachtelte Folder-Creates und nachfolgende Notizoperationen dürfen in einem Apply-Plan stehen. Folder-Move und Folder-Delete bleiben nicht unterstützt und werden im Vordergrund-Sync vor Persistenz eines Apply-Plans abgelehnt.

## Folgen

Zwei Geräte können neue Ordnerbäume und darin liegende Notizen konvergieren, ohne permanente Folder-Marker zu hinterlassen. Cursor und Baselines werden weiterhin erst nach vollständig angewendetem Plan fortgeschrieben. Folder-Move/-Delete, Konfliktmaterialisierung, Hintergrund-Scheduler und sichere Tokenpersistenz bleiben spätere Schnitte.
