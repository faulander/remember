# ADR 0020: Lokaler Delete gegen Remote-Edit mit abhängigem Tombstone-Rebase

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Client kann eine Notiz lokal löschen, während ein anderes Gerät dieselbe Notiz seit der gemeinsamen Basis bearbeitet oder verschoben hat. Die lokale Löschabsicht soll wirksam bleiben, die neuere kanonische Remote-Fassung muss zuvor jedoch sichtbar und synchronisiert gerettet werden. Eine Wiederholung darf weder mehrere Konfliktkopien noch mehrere Tombstone-Operationen erzeugen.

## Entscheidung

Schema v8 ergänzt jede entsprechende Konfliktmaterialisierung um eine optionale, eindeutige und unveränderliche UUIDv7 für die rebased Delete-Operation. Sie wird zusammen mit der neuen Konfliktnotiz-ID vor Dateisystempublikationen persistiert.

Bei einem `base_revision_mismatch` eines lokalen Note-Deletes lädt der Client den authentifizierten kanonischen Blob, prüft 8-MiB-Grenze, SHA-256 und ursprüngliche Note-ID und transformiert ihn mit neuer Note-ID und Herkunftsmetadaten. Kanonische Zwischen-Updates und -Moves bis zur gespeicherten Konfliktrevision werden im Apply-Journal verarbeitet, aber nicht am lokal bereits gelöschten Original veröffentlicht.

Nach bestätigter kanonischer Baseline wird die gerettete Fassung unter `_Konflikte/Wiederhergestellt` exklusiv veröffentlicht und durch Reconcile als normale Create-Operation erfasst. Anschließend erfolgen Materialisierungsabschluss und Enqueue des rebased Deletes in derselben SQLite-Transaktion. Das Delete basiert exakt auf der kanonischen Konfliktrevision und hängt zwingend von der Create-Operation der geretteten Konfliktnotiz ab. Der Server kann den Tombstone daher erst akzeptieren, nachdem die gerettete Fassung dauerhaft synchronisiert wurde.

Abstürze vor technischem Staging, nach sichtbarer Veröffentlichung, nach Reconcile oder unmittelbar vor Rebase nehmen dieselben persistierten Note- und Operations-IDs wieder auf. SQL-Trigger verhindern eine Änderung der Rebase-Identität.

## Folgen

Delete-vs-Edit ist nun in beiden Richtungen verlustfrei: Ein Remote-Tombstone gewinnt gegen einen lokalen Edit gemäß ADR 0019; ein lokaler Delete wird nach Sicherung des Remote-Edits auf dessen kanonische Revision rebased. Pfadkollisionen sowie Move-/Folder-Konflikte außerhalb dieser Note-Delete-Abfolge bleiben separate Konfliktklassen.
