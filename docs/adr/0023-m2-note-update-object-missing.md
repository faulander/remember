# ADR 0023: Note-Update gegen fehlendes Remote-Objekt

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Client kann auf Basis eines früher synchronisierten Objekts lokale Notizänderungen erfassen, obwohl der Server für dessen UUID keinen Zustand mehr besitzt. Der Server lehnt das Update mit `object_missing` und `canonical: null` ab. Die lokalen Bytes dürfen weder als unerfüllbare Update-Absicht verbleiben noch durch eine künstliche Wiederverwendung der verwaisten UUID auf dem Server erscheinen.

## Entscheidung

Ausschließlich ein Note-Update mit `object_missing` und fehlendem kanonischem Zustand wird materialisiert. Der Client sichert die exakten aktuellen Markdown-Bytes, erzeugt eine neue UUID und versieht die Konfliktkopie mit Herkunftsmetadaten. `canonical_revision: 0` ist neben `path_collision` ausschließlich für diesen Konfliktgrund zulässig.

Die verwaiste Originaldatei wird hashgebunden und crash-resumierbar in den deterministischen technischen Konflikt-Trash evakuiert. Nach jedem erfolgreichen oder wiederaufgenommenen Evakuierungsweg läuft Reconcile mit selektiver Remote-Delete-Unterdrückung für die alte UUID, damit kein falscher Folge-Delete entsteht. Ein Neustart mit aktiver Evakuierung bewahrt den letzten Snapshot und führt kein allgemeines Reconcile aus, bevor Sync die Unterdrückung sicher anwenden kann.

Vor sichtbarer Veröffentlichung muss die alte UUID aus dem lokalen Index verschwunden sein und der technische Trash weiterhin exakt den gesicherten Quellhash enthalten. Zusätzlich muss der server-provisionierte Wiederherstellungsordner eine dauerhafte Baseline besitzen. Die neue Konfliktnotiz wird danach als normale Create-Operation synchronisiert; die verwaiste UUID bleibt ausschließlich in der unveränderlichen Konflikthistorie referenziert.

## Folgen

Lokale Inhalte überleben einen fehlenden Remote-Objektzustand sichtbar und mit neuer serverfähiger Identität. Abstürze zwischen Dateisystemevakuierung und Snapshot-Unterdrückung sind wiederaufnehmbar. Folder-`object_missing` und weitere Strukturkonflikte bleiben getrennt.
