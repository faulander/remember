# Manueller Abnahmetest: lokaler macOS-Client

## Voraussetzung

```bash
cd client
wails build -clean
open build/bin/Remember.app
```

Für Konflikttests zusätzlich den gewählten Notizordner im Finder oder Terminal öffnen. Testdaten vorher sichern.

## Ordner und Notizen

- [x] Einen neuen leeren Ordner wählen und initialisieren.
- [x] Eine Notiz im Hauptordner erstellen; die `.md`-Datei enthält `remember.schema`, eine UUIDv7 und den eingegebenen Body.
- [x] Einen vorhandenen Unterordner im Finder anlegen, aktualisieren und eine Notiz dort erstellen.
- [x] Über den Ordner-Button einen Ordner im Hauptordner erstellen.
- [x] Per Rechtsklick auf einen Ordner dort eine Notiz und einen Unterordner erstellen; `Shift+F10` bietet dieselben Aktionen.
- [x] Eine Notiz ohne Namen erstellen; sie heißt `Neue Notiz.md`, weitere namenlose Notizen erhalten eine freie laufende Nummer.
- [x] Linker Ordnerbaum zeigt Hauptordner, verschachtelte und leere Ordner korrekt; Ordner lassen sich ein- und ausklappen.
- [x] Ein Tagfilter zeigt nur passende Notizen und deren übergeordnete Ordner.
- [x] Notiz bearbeiten und mit `⌘S` speichern; Änderung ist unmittelbar in der Datei sichtbar.
- [x] Notiz umbenennen und zwischen vorhandenen Ordnern verschieben; UUID bleibt identisch.
- [x] Eine gleichnamige Zieldatei anlegen; Erstellen/Verschieben überschreibt sie nicht.
- [x] Löschen bestätigen; Datei verschwindet aus dem Baum und liegt unter `.remember/trash`.
- [x] Anwendung neu starten und denselben Remember-Ordner öffnen.

## Markdown-Vorschau

- [x] Überschriften, Listen, Tabellen, Zitate, Links und Code werden in Vorschau und geteilter Ansicht dargestellt.
- [x] `<script>`, `onclick`, `javascript:` und `data:` werden nicht aktiv.
- [x] Remote-, Data-, `file:`- und relative Bilder werden nicht geladen oder angezeigt.
- [x] Erlaubte HTTP(S)-Links öffnen getrennt von der App.

## Tags

- [x] Tags hinzufügen, per `Enter` oder Komma übernehmen, speichern und nach Neustart wiederfinden.
- [x] Tags entfernen und Notizen über die Tagleiste filtern.
- [x] Groß-/Kleinschreibungs-Duplikate sowie NFC-äquivalente Tags werden nur einmal gespeichert.
- [x] Tags mit mehr als 40 Zeichen beziehungsweise 80 UTF-8-Bytes werden abgelehnt.

## Ungespeicherte Änderungen, externe Änderungen und Sicherheit

- [x] Editor ändern, aber nicht speichern: „Neue Notiz“ ist deaktiviert und erklärt, dass zuerst gespeichert oder verworfen werden muss.
- [x] Editor ändern und „Löschen“ wählen: Der Bestätigungsdialog weist ausdrücklich auf das Verwerfen der ungespeicherten Änderungen hin; „Abbrechen“ erhält den Puffer.
- [x] Editor ändern und das App-Fenster schließen beziehungsweise `⌘Q` auslösen: macOS zeigt die native Warnung; Abbrechen erhält den Puffer.
- [x] Editor ändern, aber nicht speichern; anschließend Datei extern ändern. Der Editorpuffer bleibt stehen und ein Konflikthinweis erscheint.
- [x] Datei extern löschen/verschieben, während sie schmutzig ist. Der Puffer bleibt kopierbar.
- [x] Datei nach dem Öffnen extern ändern und danach speichern. Externe Daten werden nicht überschrieben.
- [x] Während Speichern/Verschieben mehrfach schnelle externe Änderungen auslösen: Eine ältere Mutationsantwort darf keinen bereits neueren Watcher-Zustand oder Editorinhalt zurücksetzen.
- [x] Während Speichern die Datei am selben Pfad ersetzen: Ersatzdatei bleibt unverändert; die verdrängte Fassung bleibt bei Konflikt als `.remember-save-recovery-*` erhalten.
- [x] Während Verschieben/Löschen am Quellpfad eine neue Datei anlegen: Die neue Datei darf niemals entfernt werden; die tatsächlich erfasste Fassung wird verschoben oder als `.remember-move-recovery-*` erhalten.
- [x] Symlink als Notiz oder als Zielordner anlegen. Öffnen und Schreiben werden abgelehnt.
- [x] Auf macOS testweise Notiz-Elternordner beziehungsweise `.remember/trash` während einer Operation durch einen Symlink auf einen externen Ordner ersetzen: Außerhalb des Roots wird keine Datei erstellt, verändert oder entfernt.
- [x] Eine Datei größer als 8 MiB wird nicht in den Editor geladen.

## Darstellung und Bedienung

- [x] `System`, `Hell` und `Dunkel` testen; `System` folgt einer laufenden macOS-Umschaltung.
- [x] Remember, Nord, Dracula, Solarized und Catppuccin jeweils hell und dunkel prüfen.
- [x] Auswahl bleibt nach Neustart erhalten; manipulierte Local-Storage-Werte fallen auf Remember/System zurück.
- [x] Tastaturnavigation, sichtbare Fokusmarken, Dialog-Fokus, `Escape`, `⌘S` und kleines Fenster prüfen.
- [x] Bei aktivierter Option „Bewegung reduzieren“ entstehen keine unnötigen Animationen.
