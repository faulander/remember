# ADR 0001: Lokales Format und portable Namen in Meilenstein 1

- **Status:** Akzeptiert für Implementierung; Pfadgrenzen vor öffentlicher Freigabe manuell zu bestätigen
- **Datum:** 2026-08-01
- **Bezug:** `DATA-004`, `DATA-007`, `DATA-009`, `DATA-010`, `NAME-001`–`NAME-006`

## Kontext

Meilenstein 1 benötigt stabile Notizidentitäten, portable Pfade und konservatives Verhalten bei beschädigten oder unklaren lokalen Daten. Die Detailwerte waren im Design bewusst bis zur Implementierung vertagt.

## Entscheidungen

### Frontmatter v1

```yaml
---
remember:
  schema: 1
  note_id: "019..."
---
```

- `schema` ist ein YAML-Integer und in dieser Version exakt `1`.
- `note_id` ist eine kanonische lowercase UUID mit Bindestrichen.
- Neue IDs sind UUIDv7 nach RFC 9562.
- Der Parser akzeptiert bestehende, nichtleere RFC-4122-/RFC-9562-UUIDs einschließlich UUIDv4.
- Ein fehlender `remember`-Block darf ergänzt werden.
- Ungültiges YAML, doppelte Schlüssel, ein nicht-mappingförmiger `remember`-Wert, ungültige IDs und unbekannte Schemaversionen werden gemeldet und niemals automatisch überschrieben.
- Eine bereits gültige Identität wird nicht neu serialisiert.

### YAML-Erhaltung

- Der Client verwendet `go.yaml.in/yaml/v3` über eine lokale Adapter-API.
- Geändert wird nur der `remember`-Mapping-Knoten.
- Unbekannte Top-Level- und `remember`-Felder sowie Kommentare werden soweit von `yaml.Node` unterstützt erhalten.
- Ohne notwendige Änderung bleiben die Eingabebytes exakt erhalten.
- Bei einer Änderung bleiben Markdown-Body und ursprünglicher LF-/CRLF-Stil erhalten; YAML-Whitespace darf normalisiert werden.
- Nach Parse- oder Schemafehlern wird nicht geschrieben.

### Namensvergleich v1

- Namen müssen gültiges UTF-8 und Unicode NFC sein.
- Kollisionsschlüssel: `NFC → Unicode Default Case Fold → NFC`, ohne locale-spezifische Sonderregeln.
- Die Policy trägt Version `1`.

### Kandidaten für portable Grenzen v1

- Komponentenlänge: höchstens 180 UTF-8-Bytes nach NFC.
- Logischer relativer Pfad: höchstens 768 UTF-8-Bytes, `/` als Separator mitgezählt.
- Reserve für Konfliktsuffixe: 64 Bytes; Konfliktnamen kürzen bei Bedarf ausschließlich den Basisnamen und erhalten `.md`.
- Keine künstliche Tiefengrenze.

Diese Werte sind konservative Implementierungsgrenzen. Sie werden vor öffentlicher Freigabe auf realen Windows-, macOS- und Linux-Systemen bestätigt oder ausschließlich restriktiver geändert.

### Reservierte logische Namen

Case-folded reserviert sind:

- `.remember` nur direkt unter dem verwalteten Root,
- `_Konflikte` nur direkt unter dem Root,
- `Wiederhergestellt` nur direkt unter dem Konflikt-Root.

Gleichlautende Namen in anderen Ordnern bleiben zulässig. Der fachliche Konfliktbereich wird durch privilegierte interne Operationen angelegt; normale Benutzerpfadvalidierung lehnt seine reservierten Positionen ab.

### Ordneridentität nach Indexverlust

- Bestehenden Ordnern mit früherer Sync-Historie werden offline keine neuen UUIDs zugewiesen.
- Online darf eine Identität nur anhand widerspruchsfreier Servermetadaten und eindeutiger bekannter Notiz-Nachfahren wiederhergestellt werden.
- Leere Ordner dürfen nur über einen exakten, widerspruchsfreien Serverpfad zugeordnet werden.
- Mehrere Kandidaten, widersprüchliche Nachfahren oder parallele lokale Pfade erzeugen ein sichtbares Strukturproblem.
- Nur ein nachweislich noch nie synchronisierter neuer Baum erhält lokal neue Ordner-UUIDs.

## Konsequenzen

- Ungültige externe Namen und Dateien werden erkannt, aber nicht still verändert.
- Pfadgrenzen und Watcher-Verhalten benötigen weiterhin reale Drei-Plattform-Tests.
- YAML-Formatierung kann sich beim erstmaligen Einfügen einer Identität ändern; Inhalt und unbekannte Werte bleiben erhalten.
- Die konservative Ordnerrekonstruktion kann auf Servermetadaten warten, verhindert dafür stille Identitätsverdopplung.
