# ADR 0014: Inode-gebundener Folder-Move/-Delete-Apply

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Ordner besitzen keine permanente Identitätsdatei. Für Remote-Move und -Delete darf deshalb weder ein veralteter Indexpfad noch ein nachträglich ausgetauschtes Verzeichnis als ausreichender Identitätsnachweis gelten. Gleichzeitig müssen Abstürze zwischen Dateisystemmutation, Reconcile und Cursorfortschritt wiederaufnehmbar bleiben.

## Entscheidung

Schema v4 speichert für jede beobachtete Ordneridentität Geräte-/Inode-Werte und bindet jeden Folder-Move/-Delete-Schritt vor der Mutation unveränderlich in `apply_folder_mutations` an Plan, Schritt, Folder-ID, Quellpfad, technisches Ziel und Geräte-/Inode-Identität. Migrierte v3-Ordner ohne frühere Inode-Beobachtung werden nicht anhand ihres Pfades übernommen: Sie bleiben bis zur sicheren Identitätsauflösung `pending` und erzeugen dabei insbesondere keine ausgehende Löschabsicht.

Darwin/Linux verschieben den Quellordner zuerst descriptor-verankert und exklusiv auf einen technischen Recovery-Namen im Quellverzeichnis. Erst nach Prüfung der persistierten Geräte-/Inode-Bindung wird dieser Eintrag per No-Replace-Rename am Ziel veröffentlicht. Ein Absturz nach dem Staging oder nach der Veröffentlichung wird anhand derselben Inode-Bindung fortgesetzt. Windows bleibt bis zu handle-sicherem Reparse-Point-Schutz geschlossen.

Reconcile erhält Remote-Folder-Moves und -Deletes nur zusammen mit einem Identitätsverifier. Dieser wird vor Scan, vor Snapshot-Aufbau, vor Sync-Capture und nach Reconcile ausgeführt. Währenddessen werden keine fehlenden Note-IDs geschrieben. Ein nach Veröffentlichung ausgetauschtes Ziel führt zum Snapshot-Rollback und niemals zur Übernahme der Remote-UUID.

Folder-Delete staged zunächst nur den exakt gebundenen Ordner und entfernt ihn anschließend mit einem atomaren POSIX-`rmdir`. Dessen abschließende Leerheitsprüfung kann nicht durch ein konkurrierendes Schreiben über ein offenes Verzeichnis-Handle unterlaufen werden. Nichtleere Ordner lassen den Apply geschlossen fehlschlagen und werden an den Quellpfad zurückgestellt; dadurch werden lokale, noch nicht materialisierte Konfliktinhalte nicht unsichtbar entfernt. Kindobjekte müssen vorher im selben oder einem früheren Plan gelöscht worden sein.

## Folgen

Folder-Create, -Move und -Delete sowie enthaltene Notizoperationen sind im Vordergrund-Sync crash-resumierbar. Cursor und Baselines wechseln weiterhin erst nach vollständig angewendetem Plan. Verwaiste technische Recovery-Einträge bleiben bei konkurrierenden Ersetzungen bewusst zur manuellen Rettung erhalten. Konfliktmaterialisierung, Hintergrund-Scheduler und sichere Tokenpersistenz bleiben spätere Schnitte.
