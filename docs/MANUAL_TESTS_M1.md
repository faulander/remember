# Manuelle Tests für Meilenstein 1

Diese Checkliste prüft die Teile des lokalen Datenkerns, die automatisierte Tests und Cross-Compilation nicht ausreichend abdecken. Sie ist auf je einem realen Windows-, macOS- und Linux-System auszuführen.

Der Harness `client/cmd/remember-dev` ist ausschließlich für Entwicklung und Tests bestimmt.

## 1. Vorbereitung

Auf jedem System:

```bash
cd client
go test ./...
go test -race ./...
```

Ein leeres Testverzeichnis außerhalb des Repositorys anlegen. Danach initialisieren:

```bash
go run ./cmd/remember-dev init /absoluter/pfad/zum/testverzeichnis
```

Unter PowerShell beispielsweise:

```powershell
go run ./cmd/remember-dev init C:\Temp\remember-m1
```

Erwartet:

- `.remember/index.db` und `.remember/lock` entstehen.
- vorhandene `.md`-Dateien erhalten `remember.schema: 1` und eine UUIDv7 als `note_id`.
- Markdown-Body und unbekannte Frontmatter-Felder bleiben erhalten.
- die JSON-Ausgabe enthält Objekte, UUIDs, Identitätszustände und lokale Probleme.

## 2. Laufenden Watcher starten

In Terminal A:

```bash
cd client
go run ./cmd/remember-dev watch /absoluter/pfad/zum/testverzeichnis
```

Terminal B beziehungsweise einen externen Editor für die folgenden Schritte verwenden. Terminal A muss nach jeder Änderung ein `reconciled`-Ereignis ausgeben.

## 3. Notizen

1. `Neue Notiz.md` extern erstellen.
2. Prüfen, dass Frontmatter ergänzt wird und der Body unverändert bleibt.
3. Text extern ändern.
4. Datei in `Umbenannt.md` umbenennen.
5. Datei in einen Unterordner verschieben.
6. Datei löschen.

Erwartet:

- UUID bleibt bei Bearbeitung, Umbenennung und Verschiebung gleich.
- Hash ändert sich nur bei Inhaltsänderung.
- gelöschte Datei verschwindet aus dem lokalen Index.
- keine temporäre `.remember-write-*`-Datei bleibt zurück.

## 4. Ordner

1. Einen leeren Ordner erstellen und seine UUID notieren.
2. Den leeren Ordner bei laufendem Watcher umbenennen; UUID muss gleich bleiben.
3. Einen verschachtelten Baum mit einer Notiz erstellen.
4. Den gesamten obersten Ordner umbenennen oder verschieben.
5. Prüfen, dass die UUIDs aller eindeutig zuordenbaren Ordner und Notizen erhalten bleiben.
6. Einen Ordner kopieren, das Original löschen und beide Kopien mit identischen Notiz-IDs belassen.

Erwartet:

- echte Watcher-Moves erhalten leere Ordneridentitäten.
- verschachtelte eindeutige Moves erhalten alle Identitäten.
- mehrdeutige Kopien erzeugen `ambiguous_folder_identity` beziehungsweise `duplicate_note_id`; es wird keine Identität geraten.

## 5. Watcher-Ausfall und Reconcile

1. `remember-dev watch` beenden.
2. Mehrere Dateien und Ordner extern erstellen, bearbeiten, verschieben und löschen.
3. Watcher erneut starten.

Erwartet:

- der verpflichtende Startup-Rescan findet alle Änderungen.
- keine Änderung hängt von einer vollständigen Watcher-Ereignisfolge ab.
- unklare leere Ordner-Moves werden sichtbar gemeldet und nicht geraten.

## 6. Ungültige Namen

Soweit das jeweilige Dateisystem die Anlage erlaubt, testen:

- `CON.md`, `NUL`, `COM1.txt`, `CONIN$`,
- Namen mit abschließendem Punkt oder Leerzeichen,
- `.REMEMBER` neben `.remember` auf einem case-sensitiven Dateisystem,
- `Note.md` und `note.MD` als Geschwister auf einem case-sensitiven Dateisystem,
- nicht-NFC-normalisierte Unicode-Namen.

Erwartet:

- ungültige externe Namen bleiben byte- beziehungsweise namensgleich bestehen.
- sie erscheinen als `invalid_name` oder `name_collision`.
- sie erhalten keinen still korrigierten Namen.

Ein Betriebssystem darf Namen bereits vor Remember ablehnen; dies ist als Plattformresultat zu dokumentieren.

## 7. Kandidaten für Pfadgrenzen

Testen:

- Komponente mit genau 180 UTF-8-Bytes: akzeptiert,
- Komponente mit 181 UTF-8-Bytes: lokales Problem,
- relativer logischer Pfad mit genau 768 UTF-8-Bytes: akzeptiert, sofern das Betriebssystem ihn darstellen kann,
- Pfad mit 769 UTF-8-Bytes: lokales Problem,
- tiefe Verschachtelung unterhalb der Bytegrenze.

Zu dokumentieren:

- ob Erstellung, Lesen, atomarer Replace und Watcher-Ereignisse funktionieren,
- ab welcher Betriebssystem-/Installationspfadgrenze Fehler auftreten,
- ob die ADR-Grenzen bestätigt oder restriktiver gesetzt werden müssen.

## 8. Indexverlust

1. Watcher beenden.
2. Sicherstellen, dass Notizen UUIDs besitzen.
3. `.remember/index.db`, `index.db-wal` und `index.db-shm` löschen, aber `.remember` behalten.
4. `remember-dev status <root>` ausführen.
5. Befehl schließen und erneut ausführen.

Erwartet bei beiden Aufrufen:

- Notizidentitäten werden aus Frontmatter rekonstruiert.
- bestehende Ordner erhalten ohne Servermetadaten keine neuen UUIDs.
- für Ordner erscheint dauerhaft `ambiguous_folder_identity`.
- Recovery-Modus bleibt über Neustarts erhalten.

## 9. Sperre und Prozessende

1. Watcher in Terminal A laufen lassen.
2. In Terminal B `remember-dev status <root>` starten.
3. Watcher mit Ctrl-C beenden.
4. Status erneut starten.

Erwartet:

- paralleler zweiter Prozess meldet `remember root is already open`.
- nach sauberem Prozessende kann das Root sofort erneut geöffnet werden.
- nach erzwungenem Prozessabbruch bleibt keine dauerhaft blockierende Sperre zurück.

## 10. Wails-Oberfläche

Produktionsbuild erzeugen und starten:

```bash
cd client
wails build -clean
```

macOS:

```bash
open build/bin/Remember.app
```

In der Oberfläche prüfen:

1. nativen Ordnerdialog öffnen und abbrechen,
2. einen neuen Ordner auswählen und initialisieren,
3. einen vorhandenen Remember-Ordner öffnen,
4. externe Dateiänderungen beobachten, ohne manuell zu aktualisieren,
5. ungültige Namen und mehrdeutige Ordner als sichtbare Probleme prüfen,
6. „Aktualisieren“ und „Ordner wechseln“ verwenden,
7. Fenster während laufender Dateiänderungen schließen und erneut öffnen,
8. Tastaturfokus, Textskalierung und schmale Fensterbreite prüfen.

Erwartet:

- die UI greift nur über Wails-Bindings auf den lokalen Kern zu,
- alte Watcher-Ereignisse stellen nach einem Ordnerwechsel keinen vorherigen Zustand wieder her,
- ein fehlgeschlagener Ordnerwechsel lässt den bisherigen Root aktiv,
- Probleme zeigen lokale Pfade, aber keine Inhalte,
- App-Ende gibt Root-Sperre und Watcher zuverlässig frei.

## 11. Lokaler Editor, Vorschau, Tags und Themes auf macOS

Produktions-App starten und mit einem Test-Root öffnen:

1. Notiz im Hauptordner und in einem bestehenden Unterordner erstellen.
2. Body bearbeiten, mit `Cmd+S` speichern und extern prüfen, dass UUID und unbekannte Frontmatter-Felder erhalten bleiben.
3. Tags mit Leerraum, Unicode und unterschiedlicher Groß-/Kleinschreibung hinzufügen. Prüfen, dass getrimmt, NFC-normalisiert und case-folded dedupliziert wird.
4. Tagfilter verwenden und nach einem Neustart prüfen, dass Tags aus den Markdown-Dateien rekonstruiert werden.
5. Notiz umbenennen und in einen bestehenden Ordner verschieben; Zielkollision muss ohne Überschreiben scheitern.
6. Während ungespeicherter Editoränderungen dieselbe Datei extern ändern beziehungsweise löschen. Der Puffer muss erhalten bleiben und die UI muss den Konflikt anzeigen.
7. Notiz löschen, bestätigen, dass sie aus der Liste verschwindet und unter `.remember/trash` mit gleicher UUID wiederherstellbar liegt.
8. Bearbeiten, Vorschau und geteilte Ansicht mit Überschriften, Listen, Tabellen, Code und Links prüfen.
9. Markdown mit `<script>`, `onclick`, `javascript:`-Link, SVG sowie Remote-, Data- und `file:`-Bild testen. Nichts Aktives oder kein Bild darf ausgeführt beziehungsweise geladen werden.
10. Darstellung `System`, `Hell`, `Dunkel` sowie Remember, Nord, Dracula, Solarized und Catppuccin durchschalten. Neustart und live Systemwechsel prüfen.
11. Tastaturfokus, VoiceOver-Beschriftungen, 200-%-Textskalierung, schmale Fensterbreite und reduzierte Bewegung prüfen.

Erwartet:

- keine externe Änderung wird still überschrieben,
- Markdown-Body erscheint nie in globalen Watcher-Ereignissen,
- alle Theme-/Moduskombinationen bleiben lesbar und die Auswahl lokal erhalten,
- Vorschau führt kein Notiz-HTML aus und lädt keine Bilder,
- Löschen bleibt lokal recoverbar und löscht keine Ordner.

## 12. Ergebnisprotokoll

Pro Plattform festhalten:

- Betriebssystem und Version,
- Dateisystem,
- Go-Version,
- alle abweichenden Watcher-Ereignisse,
- Pfadgrenzresultate,
- fehlgeschlagene Szenarien mit redigierten Pfaden,
- Entscheidung: ADR-Grenzen bestätigt oder Änderung erforderlich.

Meilenstein 1 kann erst nach diesen realen Headless- und UI-Tests auf allen drei Zielplattformen vollständig abgenommen werden.
