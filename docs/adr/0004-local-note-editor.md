# ADR 0004: Lokaler Notizeditor, Tags und Darstellung

- **Status:** Akzeptiert
- **Datum:** 2026-08-03
- **Bezug:** M1 Local-first-Dateiformat und Desktop-UX

## Entscheidung

Der Desktop-Client bearbeitet echte Markdown-Dateien ausschließlich über typisierte Wails-Bindings. Der globale UI-Zustand enthält Pfade und Tags, aber keinen Markdown-Body. Beim Öffnen erhält der Editor Body, Tags, stabile Notiz-ID und eine SHA-256-Inhaltsrevision.

Speichern, Verschieben und Löschen verwenden diese Revision als optimistische Sperre. Eine externe Änderung wird niemals still überschrieben. Ein schmutziger Editorpuffer bleibt bei Watcher-Ereignissen erhalten und wird als veraltet beziehungsweise extern gelöscht markiert.

## Tags in Frontmatter v1

Tags sind optional und liegen im bestehenden `remember`-Mapping:

```yaml
remember:
  schema: 1
  note_id: "018f4c3a-1234-7abc-8123-123456789abc"
  tags:
    - Arbeit
    - Wichtig
```

Regeln:

- YAML-Sequenz aus Strings; fehlend und leere Sequenz bedeuten keine Tags.
- UI-Eingaben werden getrimmt und nach NFC normalisiert.
- Persistierte Tags müssen gültiges UTF-8 und bereits NFC sein.
- 1 bis 40 Unicode-Codepoints und höchstens 80 UTF-8-Bytes.
- Keine Unicode-Steuerzeichen.
- Eindeutig per Unicode Case Folding; erste Schreibweise und Reihenfolge bleiben erhalten.
- Keine Hierarchie, Farben, serverseitigen Tag-Objekte oder implizite Providerregeln.

Unbekannte YAML-Felder und Kommentare bleiben über `yaml.v3`-Nodes soweit technisch möglich erhalten. Bei einer Editoränderung darf YAML-Formatierung kanonisiert werden. Identitätsmetadaten werden nicht im Editor angezeigt und eine abweichende Notiz-ID wird abgelehnt.

## Dateioperationen

- Nur portable relative Benutzerpfade mit `.md` werden akzeptiert.
- Absolute Pfade, Traversal, reservierte Bereiche, Symlink-Ziele und Symlink-Eltern werden abgelehnt.
- Notizen sind auf 8 MiB inklusive Frontmatter begrenzt.
- Auf Darwin/Linux werden Root und alle Elternkomponenten über `openat` mit `O_NOFOLLOW` verankert; Dateioperationen verwenden ausschließlich `*at`-Aufrufe an diesen Deskriptoren. Ein gleichzeitiger Symlink-Tausch kann dadurch nicht außerhalb des Roots schreiben.
- Speichern verschiebt zuerst exakt das aktuelle Pfadobjekt auf einen versteckten Recovery-Namen, prüft danach die erwarteten Bytes und publiziert exklusiv. Wird der Originalpfad gleichzeitig neu angelegt, bleibt er unangetastet und die verdrängte Fassung als `.remember-save-recovery-*` erhalten.
- Verschieben und Löschen verschieben ebenfalls zuerst exakt das Quell-Pfadobjekt auf einen versteckten Staging-Namen. Ein danach neu angelegter Quellpfad wird niemals entfernt; bei nicht sicher restaurierbaren Konflikten bleibt `.remember-move-recovery-*` erhalten.
- Neue Ordner werden exklusiv unter einem vorhandenen realen Elternordner erstellt und überschreiben keine bestehenden Objekte.
- Windows verwendet dieselbe Source-Staging- und No-Clobber-Reihenfolge mit Reparse-Point-Prüfung; die handle-relative Härtung bleibt plattformspezifisch.
- Löschen ist recoverbar: Die Notiz wird mit eindeutiger Bezeichnung nach `.remember/trash` verschoben. Ordner werden nicht gelöscht.
- Jede erfolgreiche Mutation reconciled den lokalen Index unmittelbar; Watcher bleiben zusätzliche Trigger.

## Markdown-Vorschau

GitHub-Flavored Markdown wird mit `marked` gerendert und anschließend mit DOMPurify sanitisiert. Skripte, Event-Handler, aktive Einbettungen, SVG/MathML, Styles und sämtliche Bilder werden entfernt. Dadurch lädt die Vorschau weder Remote-, Data- noch lokale Dateisystembilder. Nur HTTP(S)-, `mailto:`- und Fragment-Links bleiben klickbar und erhalten `target="_blank"` sowie `rel="noopener noreferrer nofollow"`.

## Darstellung

Die Modi `System`, `Hell` und `Dunkel` sowie die eingebauten Themes Remember, Nord, Dracula, Solarized und Catppuccin werden lokal gespeichert. `System` reagiert live auf die Betriebssystemeinstellung. Themes nutzen semantische CSS-Variablen. Benutzerdefinierte Theme-Dateien sind ausdrücklich späterer Scope.

## Nicht enthalten

- eigener Theme-Import,
- Ordner umbenennen, verschieben oder löschen,
- lokale Bildfreigaben in der Vorschau,
- Rich-Text-/WYSIWYG-Editor,
- Cloud-Sync- oder Account-Funktionen.
