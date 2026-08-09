# Technical Design: Remember

- **Status:** Entwurf
- **Version:** 0.1
- **Zielrelease:** Öffentliche Beta
- **Letzte Aktualisierung:** 2026-02-17
- **Fachliche Quelle:** [`PRD.md`](./PRD.md)

## 1. Zweck und Geltungsbereich

Dieses Dokument beschreibt die technische Architektur von Remember und leitet sie aus den Anforderungen in [`PRD.md`](./PRD.md) ab.

Es definiert:

- System- und Modulgrenzen,
- lokale und serverseitige Datenmodelle,
- Synchronisations- und Konfliktprinzipien,
- das Reminder-System,
- Sicherheits-, Backup- und Betriebsarchitektur,
- technische Risiken und bewusst vertagte Detailentscheidungen.

Das Dokument ist kein vollständiges Implementierungshandbuch. Exakte Schemas, API-Nutzlasten und Zustandsautomaten werden vor oder innerhalb des zuständigen Meilensteins versioniert spezifiziert.

## 2. Architekturziele

### 2.1 Primäre Ziele

1. **Local-first:** Lokale Dateien bleiben jederzeit nutzbar (`G-002`, `SYNC-001`).
2. **Keine stille Datenvernichtung:** Konflikte bewahren alle Fassungen (`G-003`, `NFR-001`).
3. **Dateitransparenz:** Markdown-Dateien bleiben kanonisch und extern bearbeitbar (`G-001`, `DATA-001`, `DATA-003`).
4. **Deterministische Konvergenz:** Geräte gelangen trotz Offline-Phasen zu einem erklärbaren Zustand (`NFR-002`, `SYNC-003`).
5. **Plattformportabilität:** Ein logischer Baum ist auf Windows, macOS und Linux darstellbar (`G-005`, `NAME-001`).
6. **Schlanker Betrieb:** Ein modularer Go-Monolith, SQLite und lokaler Blob-Speicher genügen für die erste Zielgröße (`G-006`, `OPS-001`, `NFR-007`).
7. **Prüfbare Integrität:** Inhalte, Backups und Wiederherstellungen sind hash- und referenzprüfbar (`G-007`, `OPS-003`, `OPS-008`).

### 2.2 Zentrale Constraints

- Client zwingend mit Wails und Go.
- Web-UI mit TypeScript; SvelteKit ist bevorzugt.
- Backend als einzelner Go-Container.
- SQLite für serverseitige Metadaten.
- Inhaltsversionen im lokalen Linux-Dateisystem.
- Keine E2E-Verschlüsselung in V1.
- Keine Hintergrundzustellung bei geschlossenem App-Prozess.
- Frühe Builds bleiben unsigniert und werden manuell aktualisiert.
- Eine Backend-Replikation oder horizontale Skalierung ist nicht vorgesehen.

## 3. Systemübersicht

```text
┌─────────────────────────────────────────────────────────────┐
│ Desktop-Client: Windows / macOS / Linux                    │
│                                                             │
│  Svelte UI                                                  │
│      │ Wails Bindings / Events                              │
│  Go Application Layer                                       │
│      ├─ Local Repository / Atomic File Writer               │
│      ├─ Frontmatter Parser                                  │
│      ├─ Filesystem Watcher + Reconciler                     │
│      ├─ Local Index                                         │
│      ├─ Sync Engine                                         │
│      ├─ Reminder Engine                                     │
│      └─ Native Notification Adapter                         │
└─────────────────────┬───────────────────────────────────────┘
                      │ HTTPS + langlebige Online-Verbindung
                      ▼
┌─────────────────────────────────────────────────────────────┐
│ Linux-Server                                                 │
│                                                             │
│  TLS Reverse Proxy                                           │
│      │                                                       │
│  Go Backend (ein Container, modularer Monolith)              │
│      ├─ Identity / Sessions                                  │
│      ├─ Devices                                              │
│      ├─ Sync / Change Log                                    │
│      ├─ Objects / Paths / Tombstones                         │
│      ├─ Blob Repository                                      │
│      ├─ Reminder Coordination                                │
│      ├─ Account Deletion                                     │
│      ├─ Diagnostics / Metrics                                │
│      └─ Backup Coordination                                  │
│            │                       │                         │
│          SQLite                Blob Volume                   │
└─────────────────────────────────────────────────────────────┘
                      │
                      ▼
          verschlüsseltes externes Backupziel
```

## 4. Architekturentscheidungen

## 4.1 Local-first statt Server-Quelle für die Bearbeitung

Der lokale Dateibaum ist die unmittelbare Arbeitskopie. Die Anwendung liest und schreibt direkt in diesem Baum. Der Server hält synchronisierte Objektzustände und Versionen, ist aber nicht für alltägliches Lesen oder Schreiben erforderlich (`SYNC-001`, `OPS-006`).

**Konsequenzen:**

- UI-Aktionen müssen zuerst lokal erfolgreich sein.
- Netzwerkfehler dürfen lokale Änderungen nicht zurückrollen.
- Der Client braucht ein persistentes ausgehendes Änderungsjournal.
- Konfliktentscheidungen müssen ohne permanente Sperren funktionieren.

## 4.2 Dateien plus rekonstruierbarer technischer Index

Fachliche Informationen stehen in Markdown und Frontmatter. Der technische Index optimiert Zuordnung und Sync, ist aber nicht alleinige Quelle fachlicher Daten (`DATA-006`, `DATA-009`, `NFR-004`).

**Konsequenzen:**

- Indexverlust darf keine Notizinhalte oder Reminder-Definitionen vernichten.
- Ordner-UUIDs benötigen zusätzlich serverseitige Metadaten.
- Eine vollständige Offline-Rekonstruktion von Ordneridentitäten ist nicht immer eindeutig; solche Fälle werden sichtbar als Strukturkonflikt behandelt.

## 4.3 Modularer Monolith

Alle Backend-Funktionen laufen in einem Go-Prozess. Interne Module kommunizieren über Go-Schnittstellen und gemeinsame Transaktionsgrenzen, nicht über Netzwerk-RPC.

**Begründung:**

- Zielgröße bis etwa 100 aktive Nutzer,
- kleines Team,
- einfache Deployments und Backups,
- keine Anforderungen an horizontale Skalierung.

Modulgrenzen bleiben so definiert, dass eine spätere Extraktion möglich wäre, ohne sie vorwegzunehmen.

## 4.4 SQLite plus unveränderliche Blobs

SQLite speichert relationale Zustände und Referenzen. Markdown-Inhaltsversionen werden als unveränderliche SHA-256-Blobs im Dateisystem abgelegt.

**Begründung:**

- SQLite eignet sich für einen einzelnen schreibenden Backend-Prozess.
- Unveränderliche Blobs vereinfachen Deduplizierung, Integritätsprüfung und Backups.
- Große oder viele Inhaltsversionen blähen die SQLite-Datei nicht unnötig auf.

## 5. Clientarchitektur

## 5.1 UI-Schicht

Die UI wird gemäß [`ADR 0002`](./adr/0002-client-ui-stack.md) mit Svelte 5, TypeScript und Vite gebaut. Im Wails-Kontext wird sie vollständig statisch erzeugt; SSR und ein Node-Server sind nicht vorgesehen. SvelteKit wird erst geprüft, wenn konkrete Anforderungen an Routing oder dessen Load-/Layout-Konventionen entstehen.

Die UI darf keine direkten Dateisystem-, Netzwerk- oder Tokenzugriffe ausführen. Sie kommuniziert ausschließlich über typisierte Wails-Bindings und Ereignisse mit Go.

## 5.2 Go-Anwendungsschicht

Empfohlene Module:

```text
client/
  app/              Use Cases und Orchestrierung
  domain/           Notiz-, Ordner-, Reminder- und Konfliktmodelle
  repository/       Lokale Dateien und Index
  frontmatter/      Parse, Validate, Migrate, Patch
  watcher/          Plattformadapter und Event-Normalisierung
  reconcile/        Scan und Abgleich mit Index
  sync/             Outbox, Pull, Apply, Cursor
  reminders/        Expansion und Instanzzustände
  notifications/    Native Plattformadapter
  auth/             Sitzungstoken und Gerätezustand
  platform/         Keychain, Pfade, Dialoge, OS-Fähigkeiten
```

Die Domänenschicht kennt weder Wails noch konkrete Dateisystem-Watcher.

## 5.3 Lokales Stammverzeichnis

Ein Konto besitzt pro Gerät genau ein aktiv verwaltetes Stammverzeichnis. Ein späterer Wechsel des Verzeichnisses ist ein expliziter Migrationsvorgang, keine normale Umbenennung.

Vorgesehene Struktur:

```text
<root>/
  .remember/
    index.db             # lokaler technischer Index
    lock                 # Schutz gegen zwei aktive App-Instanzen
    logs/                # lokal, redigiert und begrenzt
    staging/             # atomare Zwischenoperationen
  Projekte/
    Beispiel.md
  _Konflikte/
    Wiederhergestellt/
```

Der konkrete Name des technischen Verzeichnisses ist versioniert und reserviert. Es wird nicht als Nutzerdaten synchronisiert.

Der sichtbare Konfliktbereich ist ein fachlicher, synchronisierter Bestandteil des Baums. Sein logischer reservierter Bezeichner ist sprachunabhängig; die UI kann eine lokalisierte Anzeige verwenden.

## 5.4 Lokaler Index

Für den lokalen Index eignet sich SQLite, da er transaktionale Outbox-, Cursor- und Zuordnungszustände benötigt. Er ist vom serverseitigen SQLite-Schema unabhängig.

Konzeptionelle Tabellen:

- `local_objects`: UUID, Typ, relativer Pfad, zuletzt beobachtete Dateiattribute,
- `file_versions`: Notiz-UUID, Inhalts-Hash, Frontmatter-Hash, Beobachtungszeit,
- `folder_paths`: Ordner-UUID, Eltern-UUID, lokaler Name,
- `outbox`: lokale Operations-ID, Basisrevision, Operation, Status,
- `sync_cursor`: letzter serverseitig bestätigter Änderungs-Cursor,
- `watcher_state`: plattformspezifische Marker und Scanstatus,
- `local_issues`: ungültige Namen, YAML-Fehler, doppelte IDs, unklare Moves,
- `notification_state`: ausschließlich gerätespezifische Auslieferungsdaten.

Der Index darf keine alleinige Kopie von Notiztexten oder Reminder-Definitionen sein.

## 5.5 Dateischreibvorgänge

App-interne Änderungen verwenden soweit möglich:

1. neue Datei im gleichen Dateisystem und Zielverzeichnis als temporäre Datei schreiben,
2. Inhalt vollständig flushen,
3. Frontmatter und Markdown erneut validieren,
4. atomar auf den Zielnamen umbenennen,
5. Elternverzeichnis synchronisieren, soweit die Plattform dies unterstützt,
6. beobachteten Indexzustand und lokale Outbox-Operation in **derselben lokalen SQLite-Transaktion** dauerhaft schreiben.

Ein Absturz nach dem Dateisystem-Rename, aber vor dieser SQLite-Transaktion hinterlässt einen vom Index abweichenden Dateistand. Der verpflichtende Start-/Overflow-Reconcile erkennt ihn anhand von UUID und Hash und erzeugt die fehlende Outbox-Operation idempotent. Der Index darf einen neuen fachlichen Dateistand niemals als vollständig verarbeitet markieren, ohne dass dieselbe Transaktion entweder die zugehörige Outbox-Operation oder eine bereits bestätigte Remote-Operations-ID enthält.

Der Watcher muss eigene Schreibvorgänge erkennen, ohne echte externe Folgeänderungen zu verschlucken. Dafür werden keine zeitbasierten pauschalen Ignore-Fenster verwendet, sondern erwartete Pfade, Hashes und Operations-IDs abgeglichen.

## 6. Fachliches lokales Datenmodell

## 6.1 Notiz

Eine Notiz besitzt mindestens:

- stabile UUID,
- aktuelle Inhaltsversion,
- relativen Pfad,
- Elternordner-UUID,
- Dateinamen,
- Markdown-Inhalt,
- versioniertes Frontmatter,
- null bis viele Reminder-Definitionen,
- optionale Konfliktherkunft.

Die UUID bleibt bei Umbenennung und Verschiebung stabil.

## 6.2 Ordner

Ein Ordner besitzt:

- stabile UUID,
- Elternordner-UUID,
- Namen,
- Revisionszustand,
- optionalen Tombstone,
- optionale Konfliktherkunft.

Der Root-Ordner besitzt eine feste, serverseitig bekannte UUID und kann nicht verschoben oder gelöscht werden. Leere Ordner werden als normale Objekte synchronisiert (`DATA-007`, `DATA-008`).

## 6.3 Inhaltsversion

Eine Inhaltsversion umfasst den vollständigen Byteinhalt einer Markdown-Datei. Serverseitig wird sie per SHA-256 adressiert.

Der Hash dient:

- Integritätsprüfung,
- Deduplizierung identischer Versionen,
- Erkennung bereits vorhandener Uploads,
- Backupverifikation.

Er ist keine fachliche Versionsnummer. Fachliche Kausalität wird über Objekt-Revisionsstände und Basisrevisionen beschrieben.

## 6.4 Tombstone

Ein Tombstone beschreibt die Löschung eines Objekts und enthält mindestens:

- Objekt-UUID und Typ,
- letzte bekannte Eltern-UUID und Namen,
- serverseitige Löschrevision,
- verursachende Operations-ID,
- Löschzeitpunkt für Aufbewahrungszwecke.

Tombstones dürfen erst entfernt werden, wenn die definierte Offline-Aufbewahrungsfrist und alle relevanten Sicherungsanforderungen erfüllt sind.

## 7. Frontmatter-Design

## 7.1 Grundsätze

Das Frontmatter muss:

- menschenlesbar bleiben,
- unbekannte Felder möglichst bewahren,
- eine explizite Schema-Version tragen,
- stabile UUIDs verwenden,
- deterministisch validierbar sein,
- fachliche Reminder-Zustände geräteübergreifend transportieren.

Ein vorläufiges, **nicht endgültig freigegebenes** Beispiel:

```yaml
---
remember:
  schema: 1
  note_id: "018f4c3a-..."
  reminders:
    - id: "018f4c3b-..."
      title: "Nachfassen"
      timezone: "Europe/Vienna"
      start_local: "2026-03-10T09:00:00"
      recurrence:
        rrule: "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=20"
        exclusions:
          - "2026-04-06T09:00:00"
      advance:
        - "PT15M"
      instances: {}
---
```

Das endgültige Schema wird in Meilenstein 1 für Notizidentität und in Meilenstein 3 für Reminder-Zustände separat versioniert.

## 7.2 Patchen statt Vollformatierung

Externe Nutzer können eigenes YAML-Frontmatter verwenden. Die App soll den `remember`-Namespace gezielt ändern und unbekannte Top-Level-Felder erhalten.

Eine bytegenaue Formatbewahrung für alle YAML-Konstrukte ist möglicherweise nicht realistisch. Deshalb gilt:

- unbekannte Werte semantisch erhalten,
- Kommentare und Formatierung soweit durch die gewählte YAML-Bibliothek möglich erhalten,
- vor jeder unvermeidbaren größeren Normalisierung eine getestete und dokumentierte Regel verwenden,
- niemals bei Parse-Fehlern überschreiben.

## 7.3 Ungültiges YAML

Bei ungültigem YAML:

- bleibt die Datei unverändert,
- wird ein lokales Problem angezeigt,
- werden keine Reminder aus dem fehlerhaften Bereich ausgeführt,
- wird die letzte bekannte Fassung nicht still als aktuelle Fassung hochgeladen,
- kann der Benutzer die Datei extern oder in einem sicheren Reparaturdialog korrigieren.

## 7.4 Doppelte Notiz-IDs

Doppelte IDs können durch externes Kopieren entstehen.

Vorgesehene Regel:

1. Sind zwei lokale Pfade mit derselben ID vorhanden, wird keiner still überschrieben.
2. Ist eine Datei eindeutig eine neu erstellte Kopie ohne eigene Sync-Historie, bietet die App die Vergabe einer neuen UUID an.
3. Ist die Herkunft unklar, entsteht ein sichtbares Identitätsproblem.
4. Erst nach eindeutiger Auflösung wird synchronisiert.

## 8. Portable Namen und Pfade

Die Validierung implementiert `NAME-001` bis `NAME-006` in einer gemeinsam genutzten Go-Bibliothek für Client und Backend.

Prüfschritte:

1. UTF-8-Gültigkeit,
2. Unicode-NFC-Normalisierung,
3. Verbot von Nullbyte und Pfadseparatoren,
4. Verbot Windows-reservierter Zeichen,
5. Verbot reservierter Gerätenamen unabhängig von Erweiterung und Case,
6. Verbot problematischer Endzeichen,
7. case-insensitive Vergleich unter Geschwistern,
8. Komponentenlänge,
9. logische Gesamtpfadlänge,
10. Reservierung interner Namen.

Die genauen Grenzwerte werden in Meilenstein 1 nach Tests der Wails-/Go-Pfade und Zielinstallationsorte festgelegt. Die logische Grenze muss konservativer sein als die kleinste praktisch unterstützte Plattformgrenze und Reserve für Konfliktsuffixe lassen.

Extern erzeugte ungültige Namen bleiben lokal bestehen, werden aber nicht hochgeladen. Die App zeigt den betroffenen Pfad lokal an; dieser Pfad darf nie in Telemetrie gelangen.

## 9. Dateisystem-Watcher und Reconciliation

## 9.1 Watcher-Abstraktion

Go verwendet eine plattformübergreifende Watcher-Abstraktion. Plattformereignisse werden in normalisierte Kandidaten umgewandelt:

- create,
- modify,
- remove,
- rename/move candidate,
- overflow/rescan required.

Watcher-Ereignisse sind Hinweise, keine vollständige Wahrheit. Eventverluste, Zusammenfassung mehrerer Events und Editor-spezifische Save-by-rename-Muster werden erwartet.

## 9.2 Laufende Moves

Wenn der Watcher eine zusammenhängende externe Bewegung erkennt, wird die bestehende Objekt-UUID auf den neuen Pfad übertragen. Für Notizen helfen Frontmatter-ID und Hash; für Ordner helfen bekannte Event-Paare und die IDs der enthaltenen Objekte.

## 9.3 Reconcile nach Stillstand

Beim Start oder nach Watcher-Overflow:

1. vollständigen Baum scannen,
2. portable Namen validieren,
3. Notiz-IDs und Inhalts-Hashes lesen,
4. bekannte Pfade mit dem Index vergleichen,
5. eindeutige Moves ableiten,
6. neue und gelöschte Objekte bestimmen,
7. unklare Ordneridentitäten als Strukturproblem markieren,
8. eine transaktionale Menge lokaler Operationen erzeugen.

Unklare Zustände werden nicht durch Zeitstempel oder heuristische Ähnlichkeit allein automatisch aufgelöst.

## 10. Backendmodule

## 10.1 Identity

Verantwortlich für:

- Registrierung,
- E-Mail-Verifikation,
- Argon2id-Passwortprüfung,
- Recovery-Tokens,
- Rate Limits,
- Kontostatus und Löschung.

## 10.2 Sessions und Devices

Verantwortlich für:

- kurzlebige Zugriffstokens,
- rotierbare Erneuerungstokens,
- Geräteidentität und Anzeigenamen,
- sichere Widerrufsketten,
- Liste aktiver Sitzungen.

Die genaue Tokenform wird in Meilenstein 2 festgelegt. Erneuerungstokens werden serverseitig nur gehasht gespeichert.

## 10.3 Sync

Verantwortlich für:

- Annahme idempotenter Clientoperationen,
- Prüfung von Basisrevisionen,
- Transaktionen über Objektzustand und Änderungslog,
- Erzeugung fachlicher Konflikte,
- Cursor-basierten Pull,
- Tombstone-Aufbewahrung.

## 10.4 Blob Repository

Verantwortlich für:

- SHA-256-Prüfung,
- atomare Write-before-reference-Speicherung,
- Lesen vollständiger Inhaltsversionen,
- Integritätsscans,
- verzögerte Garbage Collection.

## 10.5 Reminder Coordination

Verantwortlich für:

- Präsenz geöffneter Apps,
- Auswahl eines Online-Geräts,
- kurzlebige Zustell-Leases,
- idempotente Bestätigung einer Zustellinstanz.

Der Server ist nicht die einzige Reminder-Engine. Clients müssen offline dieselben Fälligkeiten berechnen können.

## 10.6 Backup Coordination

Verantwortlich für:

- konsistenten SQLite-Online-Snapshot,
- Ermittlung aller vom Snapshot referenzierten Blobs,
- Schutz der Backupgeneration gegen Garbage Collection,
- Export zum verschlüsselten externen Ziel,
- Vollständigkeits- und Hashprüfung.

## 11. Serverseitiges Datenmodell

Das folgende Schema ist konzeptionell. Namen und Spalten werden in Migrationen präzisiert.

### 11.1 Identität

- `users`
- `email_verifications`
- `password_resets`
- `devices`
- `sessions`
- `audit_events`

### 11.2 Synchronisation

- `objects`
  - Benutzer-ID
  - Objekt-UUID
  - Typ: note/folder
  - aktuelle Revision
  - Eltern-UUID
  - aktueller Name
  - aktueller Blob-Hash bei Notizen
  - Tombstone-Status
- `object_versions`
  - Objekt-UUID
  - Revision
  - Blob-Hash
  - verursachende Operations-ID
  - Konfliktherkunft
- `change_log`
  - monotoner mandantenspezifischer oder globaler Cursor
  - Benutzer-ID
  - Objekt-UUID
  - Revision
  - Operationstyp
- `operations`
  - Benutzer-ID
  - Geräte-ID
  - Client-Operations-ID
  - Ergebnis und zugeordnete Serverrevision
- `tombstones`
- `conflict_metadata`

### 11.3 Reminder-Koordination

- `device_presence`
- `delivery_leases`

Fachliche Reminder-Definitionen bleiben Bestandteil der Notizversion. Der Server kann sie aufgrund des V1-Sicherheitsmodells lesen, muss aber keine zweite unabhängige fachliche Wahrheit pflegen.

## 12. Blob-Speicher

## 12.1 Layout

Der interne M2-Schnitt ist in `docs/adr/0006-m2-blob-repository.md` präzisiert: maximal 8 MiB, Blob-Root `0700`, Dateien `0600`, separates Staging auf demselben Dateisystem, mandantengebundene Berechtigungen sowie vollständiger Recovery-/Startup-Audit. Öffentliche Transport-, GC-, Reparatur- und Backupfunktionen bleiben spätere Schnitte.

Kanonisches Layout:

```text
/blobs/sha256/ab/cd/abcdef...rest
```

Der Hash wird serverseitig aus den empfangenen Bytes berechnet und nicht ungeprüft vom Client übernommen.

## 12.2 Write-before-reference

Ablauf:

1. Upload in eine temporäre Datei auf demselben Dateisystem.
2. Während des Schreibens SHA-256 berechnen.
3. Datei flushen und `fsync` ausführen.
4. Atomar auf den Hashpfad verschieben, falls noch nicht vorhanden.
5. Elternverzeichnis synchronisieren.
6. Erst danach SQLite-Transaktion starten beziehungsweise fortsetzen, die den Hash referenziert.
7. Objektzustand und Änderungslog gemeinsam committen.

Ein Absturz vor dem DB-Commit erzeugt höchstens einen unreferenzierten Blob. Ein DB-Eintrag ohne dauerhaft gespeicherten Blob darf nicht entstehen.

## 12.3 Garbage Collection

GC arbeitet ausschließlich auf Blobs, die:

- von keiner aktiven Objektversion referenziert werden,
- von keiner aufbewahrten historischen Version benötigt werden,
- in keiner geschützten Backupgeneration liegen,
- älter als die großzügige Schutzfrist sind.

GC erstellt vor Löschung einen Prüfbericht und löscht in begrenzten Batches.

## 13. Synchronisationsprotokoll

## 13.1 Grundmodell

Synchronisiert werden idempotente, objektbezogene Operationen. Jede Operation enthält mindestens:

- Benutzer- und Geräteidentität aus der Sitzung,
- stabile Client-Operations-ID,
- Objekt-UUID und Typ,
- bekannte Basisrevision,
- Operationstyp,
- Ziel-Eltern-UUID und Name, falls relevant,
- Blob-Hash beziehungsweise Uploadreferenz, falls relevant,
- lokale Kausalitätsinformationen.

Der Server ordnet erfolgreiche Operationen einer neuen Revision und einem Änderungslog-Cursor zu.

## 13.2 Client-Outbox

Lokale Änderungen werden transaktional in die Outbox geschrieben. Zustände:

- pending,
- uploading_blob,
- submitted,
- acknowledged,
- conflict_materialized,
- failed_action_required.

Netzwerkfehler führen zur Wiederholung derselben Operations-ID.

## 13.3 Pull

Der Client fragt Änderungen seit seinem letzten bestätigten Cursor ab. Änderungen werden in einer stabilen Reihenfolge angewendet.

Ablauf:

1. Batch laden,
2. Blobs vorab verifizieren,
3. gegen noch nicht hochgeladene lokale Änderungen prüfen,
4. einen dauerhaften lokalen Apply-Plan mit erwarteten Vorzuständen im Index anlegen,
5. neue Dateifassungen vollständig im Staging-Bereich vorbereiten,
6. einzelne Dateisystemoperationen idempotent anwenden und ihren Abschluss journalisieren,
7. Konflikte materialisieren,
8. Indexzustand und Cursor nach Abschluss des gesamten Plans gemeinsam fortschreiben,
9. Apply-Plan und Staging-Dateien bereinigen.

Ein Cursor darf erst bestätigt werden, wenn die lokale Anwendung des Batches dauerhaft abgeschlossen ist. „Batch anwenden“ bedeutet dabei keine unmögliche gemeinsame Atomizität von Dateisystem und SQLite, sondern einen wiederaufnehmbaren, journalisierten Ablauf.

## 13.4 Crash-Recovery beim lokalen Apply

Jeder Apply-Schritt speichert Operationstyp, erwartete Objekt-/Ordner-UUID, Quell- und Zielpfad, erwartete Eltern-UUID, erwartete Existenzzustände, bei Dateien Vorher-/Nachher-Hash und Status. Nach einem Absturz wird ein unvollständiger Plan vor neuem Pull fortgesetzt oder kontrolliert kompensiert.

Vor jeder Wiederholung prüft der Client operationsspezifische Replay-Prädikate:

- **Datei erstellen/ändern:** Entspricht sie bereits UUID und Nachher-Hash, gilt der Schritt als abgeschlossen. Entspricht sie UUID und Vorher-Hash, darf er erneut angewendet werden. Jeder andere Inhalt wird als externe Konkurrenz erhalten.
- **Datei oder Ordner verschieben:** Stimmen UUID-Zuordnung, Quell-/Zielexistenz und erwartete Eltern-UUID mit dem Vorzustand überein, wird der Move wiederholt. Liegt das Objekt bereits eindeutig am Ziel, gilt er als abgeschlossen. Ein belegtes oder mehrdeutiges Ziel erzeugt einen Strukturkonflikt.
- **Datei oder Ordner löschen:** Das Objekt wird zunächst atomar in einen planbezogenen Quarantänepfad unter `.remember/staging` verschoben, nicht sofort endgültig entfernt. Fehlt es am Ursprung und liegt mit erwarteter UUID in Quarantäne, gilt der Schritt als angewendet. Abweichende Inhalte oder Nachfahren werden gerettet beziehungsweise als Konflikt behandelt.
- **Leeren Ordner erstellen:** Existenz und UUID-Pfad-Zuordnung werden gemeinsam geprüft. Ein fremdes Objekt am Ziel löst die Pfadkollisionsregel aus.

Da Ordner-UUIDs nicht im sichtbaren Verzeichnis liegen, ist die journalisierte Indexzuordnung Teil des Replay-Prädikats. Wurde der Ordner nach dem Absturz extern bewegt und ist die Zuordnung nicht mehr eindeutig, wird niemals geraten; es entsteht ein sichtbarer Strukturkonflikt.

Staging- und Quarantänedaten tragen Plan- und Operations-IDs. Sie werden erst endgültig gelöscht, nachdem Dateisystemschritte, Indexzustand und Cursor gemeinsam abgeschlossen sind. Fehler-Injektionstests decken Dateien, leere/nichtleere Ordner, Moves, Löschungen und jeden Übergang zwischen Dateisystemoperation, Indexupdate und Cursorfortschritt ab.

## 13.5 Konflikterkennung

Ein Inhaltskonflikt liegt vor, wenn:

- der Client eine Basisrevision meldet,
- die Serverrevision seit dieser Basis unabhängig verändert wurde,
- und die neue Operation nicht identisch beziehungsweise bereits idempotent verarbeitet ist.

Zeitstempel entscheiden nicht, welche Fassung fachlich gewinnt.

## 14. Konfliktmatrix

## 14.1 Bearbeiten gegen Bearbeiten

Umsetzung von `SYNC-006` und `SYNC-007`:

- Beide vollständigen Fassungen bleiben erhalten.
- Eine deterministische Regel bestimmt die Fassung am Originalobjekt.
- Die andere wird als neue Notiz mit neuer UUID materialisiert.
- Die Konfliktkopie erhält einen portablen sichtbaren Suffix.
- Beide enthalten beziehungsweise referenzieren Konfliktherkunft.

## 14.2 Löschen gegen Bearbeiten

Umsetzung von `SYNC-008`:

- Der Tombstone am ursprünglichen Pfad bleibt wirksam.
- Die bearbeitete Fassung wird als neue Notiz unter dem reservierten Wiederherstellungsbereich gespeichert.
- Die ursprüngliche Objekt-ID, Version und Geräteherkunft bleiben als Konfliktmetadaten erhalten.
- Implementierungsstand M2: Sowohl lokales Note-Update gegen kanonischen Remote-Delete als auch lokaler Delete gegen kanonischen Remote-Edit sind crash-resumierbar umgesetzt; der lokale Delete wird erst nach synchronisierter Rettung der Remote-Fassung rebased.

## 14.2.1 Gleichzeitiges Erstellen am selben Notizpfad

- Der serverseitig zuerst akzeptierte Create bleibt am ursprünglichen Pfad.
- Der verlierende Client evakuiert seine exakten Bytes in technischen Trash und materialisiert sie mit neuer UUID unter Wiederhergestellt.
- Die Konfliktkopie wird erst sichtbar, wenn der Remote-Gewinner durch Baseline und abgeschlossenen Apply-Schritt authentifiziert ist.
- Bei einer Note-Move-Kollision wird zusätzlich die Quell-UUID am authentifizierten kanonischen Pfad wiederhergestellt; lokal verschobene und abhängig bearbeitete Bytes werden mit neuer UUID gerettet.

## 14.2.2 Update gegen fehlendes Remote-Objekt

- Ein `object_missing` ohne kanonischen Zustand übernimmt niemals still die verwaiste UUID.
- Die lokalen Bytes werden mit neuer UUID unter Wiederhergestellt gerettet und die alte Quellidentität nur als Konfliktherkunft bewahrt.
- Die Evakuierung von Update oder Move unterdrückt einen falschen lokalen Folge-Delete auch über Absturz und Neustart hinweg.
- Ein lokaler Delete gegen `object_missing` ist semantisch bereits erfüllt und wird unveränderlich als `already_deleted` journalisiert, ohne ein neues Serverobjekt zu erzeugen.

## 14.2.3 Leeren Ordner löschen gegen neue Remote-Kinder

- Ein serverseitiges `folder_not_empty` verwirft die lokale Delete-Absicht zugunsten der bereits synchronisierten Kinder.
- Der fehlende Parent-Ordner wird ausschließlich aus kanonischem Zustand und über ein Nonce-/Inode-Journal wiederhergestellt.
- Erst nach identitätsgebundener Restaurierung und dauerhafter Konfliktauflösung werden die Remote-Kinder angewendet.

## 14.2.2.1 Leere Folder-Create-Pfadkollision

- Der Server-Gewinner behält den ursprünglichen Pfad.
- Ein nachweislich leerer lokaler Verlierer ohne abhängige Intents wird inode-gebunden unter neuer UUID direkt in `_Konflikte/Wiederhergestellt` verschoben und dort neu angelegt.
- Die Leere wird nach verborgenem Inode-Staging und nach Veröffentlichung geprüft; konkurrierende Inhalte stellen den Quellfolder wieder her und lassen den Konflikt fail-closed.
- Nichtleere Folder benötigen ein separates transaktionales Subtree-Rekeying.

## 14.2.3.1 Lokale Folder-Mutations-Echos

- Reconcile bindet bekannte lokale Folder-Moves und -Deletes bereits beim Outbox-Enqueue an Quellpfad sowie Device/Inode.
- Nur ein exakt passendes akzeptiertes Operation-/Revision-/Cursor-Echo darf diese Bindung übernehmen.
- Move-Ziel beziehungsweise Delete-Abwesenheit und reale Pfadidentität werden vor dem Apply-Abschluss erneut geprüft.
- Innerhalb einer Pull-Seite folgen virtuelle Notizpfade verifizierten Ancestor-Moves; Reconcile unterdrückt nur exakte ID-/Hash-/Zielpfadzustände.
- Ein im selben Plan erstellter und gelöschter Folder konsumiert seinen Nonce-Marker journalisiert und über Neustarts resumierbar.

## 14.2.3.2 Folder-Move gegen belegten Pfad, gelöschten Parent oder Zyklus

- Der konfliktbehaftete Folder wird anhand des vor dem Outbox-Enqueue gebundenen Device/Inode an seinen authentifizierten kanonischen Pfad zurückverschoben.
- Nur deterministisch mitbewegte Nachfahrenpfade werden beim Reconcile unterdrückt; externe Abweichungen bleiben lokale Intents.
- Abhängige Operationen anderer Objekte dürfen nach der unveränderlichen Auflösung `folder_move_reverted` fortfahren.
- Ein serverseitiger `folder_cycle` verwendet denselben Revert: Erst kehrt der stale lokale Folder zurück, danach wird die aktuelle Remote-Ancestry gepullt.
- Zwei konkurrierende Moves mit exakt derselben Parent-ID und demselben Namen gelten nach Inode-Prüfung als äquivalent; nur die höhere Remote-Revision wird anschließend gepullt.
- Divergente Move-Ziele bleiben fail-closed, weil der kanonische Snapshot keinen authentifizierten Pfad der gesamten Parent-Ancestry enthält.
- Spätere Operationen desselben Folders verhindern den automatischen Revert, weil ihre Basisrevision neu berechnet werden müsste.

## 14.2.4 Notiz erstellen oder verschieben gegen gelöschten Parent

- `parent_unavailable` verwirft den nicht mehr gültigen Zielpfad, nicht aber lokale Notizbytes.
- Note-Create wird als kanonisch fehlendes Objekt evakuiert und unter neuer UUID im Konfliktbereich gerettet.
- Note-Move rettet die lokale Zielfassung und stellt zusätzlich die authentifizierte kanonische Serverfassung an ihrem bisherigen Pfad wieder her.
- Während der Evakuierung bleibt allgemeines Reconcile auch über Neustarts gesperrt.

## 14.2.5 Typkonflikte

- `type_mismatch` zeigt eine beschädigte oder falsch zugeordnete lokale UUID an, nicht einen regulären konkurrierenden Edit.
- Der kanonische Serverzustand wird authentifiziert und replay-stabil gespeichert, aber nicht über das lokale Objekt anderen Typs angewendet.
- Der Client bleibt vor Pull und Apply fail-closed; Note-Bytes, Folder-Inodes und Unterbäume bleiben unverändert.
- Eine spätere automatische Reparatur benötigt ein vollständiges crash-resumierbares Rekey-Protokoll für Objekt und abhängige Intents.

## 14.3 Löschen gegen Verschieben

- Die Löschung des ursprünglichen Objekts bleibt wirksam.
- Die konkurrierend verschobene Fassung wird unter Wiederhergestellt gerettet.
- Bei Ordnern wird der gesamte erhaltene Teilbaum materialisiert, ohne bereits unabhängig gelöschte Nachfahren wiederzubeleben.

## 14.4 Umbenennen oder Verschieben gegen Umbenennen oder Verschieben

- Die stabile UUID verhindert eine Verdopplung desselben Objekts.
- Eine deterministische Operationsordnung bestimmt den kanonischen Ort.
- Die alternative Benutzerabsicht wird als Strukturkonflikt angezeigt.
- Sind unterschiedliche Objekte am gleichen Ziel beteiligt, greift die Pfadkollisionsregel.

Die genaue UX, ob die alternative Absicht zusätzlich als Alias oder nur als Konfliktereignis dargestellt wird, wird in Meilenstein 2 festgelegt.

## 14.5 Zwei Objekte am gleichen Ziel

Umsetzung von `SYNC-009`:

- Beide Objekte bleiben im gewünschten Zielordner.
- Sortierung nach stabiler Operations-ID beziehungsweise UUID bestimmt den Originalnamen.
- Das andere Objekt erhält einen Konfliktsuffix.
- Bei Längenüberschreitung wird der Basisname deterministisch gekürzt und um einen kurzen UUID-Anteil ergänzt.

Ein vorgeschlagener logischer Suffix ist:

```text
 (Konflikt - <Gerätekurzname> - <Datum> - <ID>)
```

Die tatsächliche Zeichenmenge muss den portablen Namensregeln entsprechen; typografische Gedankenstriche werden nicht vorausgesetzt.

## 14.6 Konfliktbereich

Der Bereich `_Konflikte/Wiederhergestellt` besitzt eine reservierte stabile Ordneridentität. Benutzer dürfen Inhalte daraus verschieben oder löschen, aber die reservierte Root-Bedeutung nicht durch ein normales Objekt ersetzen.

## 14.7 Sync-Fortsetzung

Konflikte werden als normale resultierende Objekte und Metadaten persistiert. Dadurch blockieren sie nicht den Sync unbeteiligter Objekte (`SYNC-012`). Nur lokal unlesbare oder nicht sicher darstellbare Objekte bleiben als gezielte Probleme ausgesetzt.

## 15. Reminder-Architektur

## 15.1 Grundmodell

Eine Reminder-Serie besitzt:

- stabile Reminder-UUID,
- Start als lokale Kalenderzeit,
- feste IANA-TZID,
- optionale Wiederholungsregel,
- Endbedingung,
- Ausnahmen,
- Vorlaufdefinitionen,
- fachliche Instanzzustände.

Die Umsetzung orientiert sich an iCalendar RRULE, übernimmt aber nur einen explizit getesteten und dokumentierten Teilumfang beziehungsweise kapselt eine etablierte Bibliothek.

## 15.2 Zeitmodell

- Lokale Startzeit und TZID sind kanonisch.
- Auslösungszeitpunkte werden mit einer versionierten IANA-Zeitzonendatenbank berechnet.
- Reisen verändern die Serie nicht (`REM-009`, `REM-010`).
- Geräte zeigen zusätzlich die umgerechnete aktuelle lokale Zeit an.

## 15.3 DST-Regeln

Vor Meilenstein 3 werden feste Regeln definiert für:

- nicht existente lokale Uhrzeiten beim Vorwärtssprung,
- doppelte lokale Uhrzeiten beim Rückwärtssprung,
- Änderungen der TZDB zwischen Geräten,
- Monats- und Jahresregeln an nicht existenten Kalendertagen.

Diese Regeln benötigen plattformunabhängige Testvektoren. Clients und Backend dürfen nicht von unterschiedlichen impliziten Betriebssystemregeln abhängen.

## 15.4 Instanzzustände

Voraussichtliche fachliche Zustände:

- scheduled,
- delivered,
- snoozed,
- completed,
- skipped,
- superseded.

Der endgültige Zustandsautomat, die Speicherung wiederkehrender Historie und Konfliktregeln werden in Meilenstein 3 festgelegt.

## 15.5 Lokale Ausführung

Solange der App-Prozess läuft:

1. Reminder-Engine expandiert das relevante Zeitfenster.
2. Sie hält monotone Timer nur als Optimierung; Systemzeitänderungen lösen eine Neuberechnung aus.
3. Fällige Instanzen werden mit stabiler Instanz-ID identifiziert.
4. Online wird eine Zustell-Lease angefragt.
5. Offline wird direkt lokal benachrichtigt.
6. Benutzeraktionen werden sofort fachlich im Frontmatter gespeichert und in die Sync-Outbox gestellt.

## 15.6 Verpasste Instanzen

Beim App-Start wird seit dem letzten erfolgreichen Lauf ein begrenztes Rückblickfenster ausgewertet. Die Produktregel für lange Serien ist noch offen. Technisch muss eine Aggregation möglich sein, damit tausende verpasste tägliche Instanzen nicht tausende Systembenachrichtigungen erzeugen.

Vor dem Exit von Meilenstein 3 wird diese Regel versioniert festgelegt. Sie definiert Rückblickgrenze, maximale Zahl einzeln dargestellter Instanzen, Aggregation pro Reminder-Serie, Sortierung und den daraus entstehenden fachlichen Zustand. Gemeinsame Testvektoren prüfen identisches Verhalten auf allen drei Plattformen (`M3-AC-005`).

## 15.7 Online-Gerätewahl

Geöffnete Apps halten eine authentifizierte langlebige Verbindung oder senden kurze Heartbeats. Der Server führt kurzlebige Presence-Daten.

Für eine fällige Instanz:

1. Geräte melden Kandidatur mit stabiler Instanz-ID.
2. Der Server wählt deterministisch ein aktives, benachrichtigungsfähiges Gerät.
3. Er erteilt eine kurze Lease.
4. Das Gerät bestätigt erst nach erfolgreicher Übergabe an den nativen Adapter die Anzeige.
5. Bleibt diese Bestätigung aus, wird nach Lease-Ablauf ein erreichbares Gerät erneut ausgewählt.
6. Verweigert das Betriebssystem die Berechtigung oder meldet der Adapter einen dauerhaften Fehler, zeigt der Client die Instanz in-app als nicht nativ zugestellt an.

Exactly-once-Zustellung ist bei Netzwerkpartitionen nicht garantiert. Ziel ist „online normalerweise einmal, bei fehlender Bestätigung erneut versuchen, offline mindestens lokal“. Lease-Failover kann seltene Duplikate erzeugen; Duplikate sind sicherer als still ausgelassene Erinnerungen. Sind alle Apps geschlossen oder alle Adapter nicht benachrichtigungsfähig, wird die Instanz beim nächsten Öffnen als verpasst behandelt (`REM-011`, `REM-012`).

## 15.8 Widersprüchliche Offline-Aktionen

Beispiele:

- Gerät A erledigt, Gerät B schlummert dieselbe Instanz.
- Beide Geräte schlummern auf unterschiedliche Zeiten.
- Ein Gerät ändert die Serie, ein anderes bearbeitet eine Instanzausnahme.

Diese Fälle dürfen nicht per blindem Last-Write-Wins entschieden werden. Der endgültige Zustandsjoin kann je Aktion semantisch sein oder eine sichtbare Reminder-Konfliktkopie erzeugen. Die Matrix wird vor Meilenstein 3 spezifiziert.

## 16. Native Benachrichtigungen

Eine Go-Schnittstelle kapselt Plattformadapter:

```go
type Notifier interface {
    Show(ctx context.Context, n Notification) (DeliveryID, error)
    Update(ctx context.Context, id DeliveryID, n Notification) error
    Close(ctx context.Context, id DeliveryID) error
}
```

Zu validieren:

- Berechtigungsdialoge,
- Klick öffnet richtige Notiz,
- Aktionen „Erledigen“ und „Schlummern“, soweit plattformübergreifend zuverlässig,
- Verhalten bei minimiertem Fenster,
- Verhalten bei Prozessende,
- Linux-Desktopvarianten.

Die App installiert keinen separaten Hintergrunddienst. Ist der Prozess beendet, erfolgt keine Echtzeitbenachrichtigung (`REM-011`).

## 17. Authentifizierung und Sitzungen

## 17.1 Passwörter

- Argon2id mit versionierten Parametern,
- zufälliges Salt pro Passwort,
- serverseitige Parameteranhebung bei erfolgreichem Login möglich,
- niemals Passwort oder Hash in Logs.

## 17.2 E-Mail-Verifikation und Recovery

Tokens sind:

- kryptografisch zufällig,
- kurzlebig,
- einmal verwendbar,
- serverseitig nur gehasht gespeichert,
- an Zweck und Konto gebunden.

Antworten und Timing werden so gestaltet, dass Account Enumeration erschwert wird.

## 17.3 Sitzungstokens

Vorgesehen:

- kurzes Zugriffstoken,
- rotierendes Erneuerungstoken,
- serverseitige Tokenfamilie pro Gerät,
- Replay-Erkennung bei wiederverwendetem Vorgängertoken,
- sofortiger Widerruf pro Sitzung oder aller Sitzungen.

Auf dem Client wird das Erneuerungstoken in Keychain, Credential Manager beziehungsweise Secret Service gespeichert. Fällt keine sichere Ablage an, darf die App nicht still auf ungeschützte Klartextspeicherung wechseln.

## 18. Autorisierung und Mandantentrennung

Jede serverseitige Repository-Methode verlangt einen Benutzerkontext. Objekt-UUID allein ist nie eine ausreichende Zugriffsberechtigung.

Datenbankzugriffe enthalten die Benutzer-ID in der WHERE-Bedingung beziehungsweise verwenden einen mandantengebundenen Repository-Typ. Tests versuchen gezielt Cross-Tenant-Zugriffe auf:

- Objekte,
- Blobs,
- Änderungslog-Cursor,
- Geräte,
- Sitzungen,
- Backup-/Löschoperationen.

Blob-Deduplizierung darf keinen Informationskanal schaffen. API-Antworten dürfen nicht offenlegen, ob ein Hash bei einem anderen Benutzer existiert.

## 19. Kontolöschung

Ablauf:

1. erneute Authentifizierung,
2. serverseitige Löschtransaktion anlegen,
3. alle Sitzungen sofort widerrufen,
4. Konto für weitere Synchronisation sperren,
5. relationale aktive Daten und Blob-Referenzen zeitnah entfernen,
6. nicht mehr referenzierte Blobs nach Schutzregeln bereinigen,
7. Auditdaten auf gesetzlich und betrieblich nötiges Minimum reduzieren,
8. Backupkopien mit Ablauf der dokumentierten Maximalfrist auslaufen lassen.

Clients erhalten keine Anweisung, normale lokale Markdown-Dateien zu löschen (`SEC-008`). Wird später mit derselben E-Mail ein neues Konto erstellt, ist ein lokaler Neuimport eine bewusste Aktion und keine automatische Wiederherstellung des gelöschten Kontos.

## 20. Transport- und Server-Sicherheit

- TLS durch Reverse Proxy mit automatisierter Zertifikatserneuerung.
- Sicherheitsheader und enge CORS-Regeln, soweit für Wails relevant.
- Größenlimits für Requests und Blobs.
- Zeit- und Mengenlimits für YAML-/Reminder-Verarbeitung.
- Rate Limits für Auth- und teure Sync-Endpunkte.
- Serverprozess ohne Root-Rechte.
- schreibbare Volumes nur für SQLite, Blobs und nötige temporäre Pfade.
- Secrets außerhalb des Images.
- verschlüsselte Serverdatenträger und Backupziele.
- strukturierte Logs ohne Inhalte oder Benutzerpfade.

## 21. Backup und Wiederherstellung

## 21.1 Backupablauf

Mindestens täglich:

1. SQLite Online Backup in ein generationiertes Staging-Ziel erzeugen.
2. Snapshot mit SQLite-Integritätsprüfung öffnen.
3. Menge aller referenzierten Blob-Hashes bestimmen.
4. Referenzierte Blobs ins Backupziel übertragen oder ihre vorhandene gesicherte Kopie verifizieren.
5. jeden Blob-Hash prüfen.
6. Manifest mit DB-Hash, Blobliste, Schema- und App-Version erzeugen.
7. Generation verschlüsselt auf getrenntes Ziel übertragen.
8. erfolgreichen Abschluss überwachen und alarmieren.

## 21.2 Konsistenz

Der SQLite-Snapshot ist der Commit-Punkt des Backups. Da Blobs unveränderlich und vor ihrer Referenz dauerhaft geschrieben werden, muss jeder referenzierte Blob bereits existieren.

Ein fehlender Blob macht das Backup ungültig. Der Job darf in diesem Fall keinen Erfolg melden.

## 21.3 Restore

Ein vollständiger Restore auf leerer Umgebung:

1. Backupmanifest und Verschlüsselung prüfen,
2. SQLite-Snapshot wiederherstellen,
3. alle referenzierten Blobs wiederherstellen,
4. Hashes prüfen,
5. DB-Migration nur nach gesichertem Original durchführen,
6. Integritätsscan ausführen,
7. Dienst in isolierter Umgebung starten,
8. Stichproben-Sync mit Testkonten durchführen.

Es besteht keine feste RTO (`OPS-005`). Der Restore-Prozess muss dennoch dokumentiert und wiederholbar sein.

## 21.4 Aufbewahrung und GC

Noch festzulegen:

- Zahl täglicher und langfristiger Generationen,
- Restore-Testfrequenz,
- maximale Backupfrist nach Kontolöschung,
- GC-Schutzfrist.

Zwingende Invariante: Kein Blob darf gelöscht werden, solange eine aufbewahrte Backupgeneration ihn zur vollständigen Wiederherstellung benötigt.

## 22. Diagnose und Observability

## 22.1 Clientdiagnose

Vor dem ersten Versand zeigt der Client Zweck, Felder und Aufbewahrung der Diagnose an. Im Pilot wird erst nach ausdrücklicher Zustimmung ein pseudonymer Diagnoseschlüssel erzeugt und serverseitig als erteilte Einwilligung registriert (`TEL-001`). Ohne Zustimmung werden keine Clientdiagnosen übertragen.

In der offenen Beta besitzt die Einstellung einen jederzeit erreichbaren Schalter (`TEL-005`). Abschalten stoppt neue Erfassung und Versand unmittelbar und verwirft noch nicht gesendete Ereignisse. Aggregierte, ohnehin serverseitig erzeugte Betriebsmetriken bleiben davon unberührt. Der Widerruf darf Synchronisation und lokale Nutzung nicht beeinträchtigen.

Zulässige Felder gemäß `TEL-002`:

- pseudonyme Geräte-/Sitzungs-ID,
- App- und OS-Version,
- redigierter Stacktrace,
- Sync-Ergebnis und Dauer,
- stabiler Fehlercode,
- Objektanzahlen.

Explizit verboten gemäß `TEL-003`:

- Markdown-Inhalte,
- Dateinamen,
- Pfade,
- Frontmatter-Werte,
- Reminder-Texte.

Redigierung erfolgt vor Versand. Automatisierte Tests speisen künstliche Geheimnisse und Pfade ein und prüfen, dass sie nicht in Diagnoseereignissen erscheinen.

Der Diagnose-Endpunkt erzwingt eine konfigurierbare maximale Aufbewahrung und trennt Schreib-, Auswertungs- und Administrationsrechte (`TEL-006`). Abgelaufene Rohereignisse werden automatisiert gelöscht; längerfristig verbleiben nur ausreichend aggregierte Metriken. Konkrete Fristen werden vor Meilenstein 4 dokumentiert und in Löschtests geprüft.

## 22.2 Servermetriken

Mindestens:

- Request- und Fehlerquote,
- Login-/Recovery-Fehler aggregiert,
- Sync-Latenz und Batchgrößen,
- Konfliktanzahlen ohne Namen/Inhalte,
- offene Verbindungen,
- SQLite-Lock-/Busy-Ereignisse,
- Blob-Integritätsfehler,
- Speicherverbrauch,
- Backupstatus und Alter des letzten erfolgreichen Backups,
- E-Mail-Zustellergebnis aggregiert.

## 22.3 Logs

Logs verwenden stabile Ereignis- und Fehlercodes. Benutzer- und Gerätekennungen werden pseudonymisiert. Pfade und Dateinamen erscheinen weder in normalen Logs noch in Telemetrie.

Lokale Supportdiagnosen können bewusst exportiert werden, benötigen aber denselben Redigierungsfilter.

## 23. Deployment

## 23.1 Container

Der Go-Backend-Container ist möglichst minimal und unveränderlich. Persistente Mounts:

```text
/data/sqlite/
/data/blobs/
/data/staging/
```

SQLite und Blobvolume müssen auf demselben zuverlässigen lokalen Linux-Dateisystem liegen, wenn atomare Rename-Annahmen dies verlangen. Netzwerkdateisysteme werden in V1 nicht vorausgesetzt.

## 23.2 SQLite-Betrieb

- WAL-Modus,
- ein Backend-Schreibprozess,
- definierter Busy-Timeout,
- kurze Schreibtransaktionen,
- Foreign Keys aktiviert,
- regelmäßige Integritätsprüfung,
- kontrollierte Checkpoints,
- Migrationen vor Dienstfreigabe und nach Backup.

Ein zweiter Backend-Container darf nicht parallel auf dieselbe SQLite-Datei schreiben.

## 23.3 Single-Server-Ausfall

Ein Serverausfall unterbricht:

- Sync,
- Anmeldung neuer Sitzungen,
- Recovery,
- Online-Gerätewahl.

Lokales Lesen, Schreiben und Offline-Reminder bleiben verfügbar. Nach Wiederherstellung synchronisieren Clients ihre Outbox.

## 24. Packaging und Releaseintegrität

## 24.1 Artefakte

Vorgesehen:

- Windows: ZIP beziehungsweise unsignierter Installer, endgültig noch festzulegen,
- macOS: unsigniertes App-Bundle im Archiv mit dokumentiertem Gatekeeper-Workaround,
- Linux: AppImage, optional später `.deb`.

## 24.2 Release-Manifest

Das Manifest enthält mindestens:

- Version,
- Veröffentlichungszeit,
- unterstützte Plattformen/Architekturen,
- Artefaktnamen und SHA-256,
- Sync-Protokollversion,
- minimale Server-/Client-Kompatibilität,
- Download-URLs,
- optionalen Sperrgrund.

Das Manifest wird mit einem getrennt geschützten Release-Schlüssel signiert. Der öffentliche Schlüssel ist im Client verankert.

## 24.3 Versionsprüfung

Beim App-Start:

1. Versionsendpunkt über TLS abfragen,
2. signiertes Manifest verifizieren,
3. aktuelle Version vergleichen,
4. normales Update anzeigen oder serverseitige Mindestversion erklären,
5. Download bleibt manuell.

Eine Sperre darf lokale Offline-Dateien nicht unzugänglich machen. Sie darf unsichere Serverkommunikation blockieren (`REL-006`).

## 25. Teststrategie

## 25.1 Unit- und Property-Tests

Besonders geeignet für:

- portable Namensregeln,
- Unicode-Normalisierung,
- Konfliktsuffixe und Längenkürzung,
- Sync-Zustandsübergänge,
- Idempotenz,
- RRULE-/DST-Testvektoren,
- Frontmatter-Migrationen,
- Hash- und Blobinvarianten.

## 25.2 Modellbasierte Sync-Tests

Ein In-Memory-Modell erzeugt zufällige Operationen mehrerer Geräte:

- erstellen,
- bearbeiten,
- verschieben,
- umbenennen,
- löschen,
- offline/online wechseln,
- Requests wiederholen,
- Reihenfolgen vertauschen.

Invarianten:

- keine stille Inhaltsvernichtung,
- eindeutige UUIDs,
- portable eindeutige Pfade,
- idempotente Wiederholung,
- alle referenzierten Blobs existieren,
- unbeteiligte Objekte konvergieren.

## 25.3 Fehler-Injektion

Tests beenden Prozesse kontrolliert zwischen:

- temporärem Dateischreiben und Rename,
- Blob-Rename und DB-Commit,
- DB-Commit und HTTP-Antwort,
- lokaler Dateianwendung und Cursorfortschritt,
- bestätigter Pull-Seite und Anforderung der Folgeseite,
- Backup-Snapshot und Blobkopie.

Nach Neustart müssen Invarianten bestehen oder ein sichtbarer Integritätsfehler entstehen.

## 25.4 Plattform-Smoke-Tests

Auf je einem realen Windows-, macOS- und Linux-System:

- Build starten,
- Stammverzeichnis wählen,
- interne und externe Dateioperationen,
- Watcher-Overflow/Reconcile,
- Zwei-Geräte-Sync über verschiedene Betriebssysteme,
- native Benachrichtigung,
- manuelle Updateanzeige,
- sichere Tokenablage,
- Deinstallation ohne Löschung des Notizbaums.

## 25.5 End-to-End-Tests

Der automatisierte Mehrgeräte-Harness verwendet produktionsnahe Identity-/Session-, Blob- und Sync-Komponenten hinter den echten HTTP-Routen. Er prüft getrennte Geräte-Tokens, Roots, Cursor, die byteidentische Update-Konfliktkonvergenz, Note-/Folder-Move/-Delete, einen Serverneustart auf denselben Datenpfaden, den kalten vollständigen History-Bootstrap und die cursor-exakte Wiederaufnahme nach Abbruch zwischen zwei Pull-Seiten. Weitere Szenarien ergänzen dieselbe Oberfläche.

- Registrierung bis Verifikation,
- Login und Tokenrotation,
- Gerätewiderruf,
- Recovery,
- Kontolöschung,
- Offline-Änderungen und Wiederaufnahme,
- alle Konfliktklassen,
- Backup und vollständiger Restore.

## 26. Entwicklungsmeilensteine

## 26.1 Meilenstein 1 – Lokaler Datenkern

Technische Ergebnisse:

- Domainmodell und lokale Repository-Schnittstellen,
- Frontmatter-Schema v1 für Notiz-ID,
- lokaler Index,
- portable Namensbibliothek,
- atomare Dateischreibvorgänge,
- Watcheradapter für drei Plattformen,
- Reconcile und lokale Problemansicht.

Exit-Kriterien: `M1-AC-001` bis `M1-AC-004`.

## 26.2 Meilenstein 2 – Identität und Synchronisation

Technische Ergebnisse:

- Backendcontainer und Migrationen,
- Auth/Sessions/Devices,
- Blob Repository,
- Sync-Outbox, Änderungslog und Pull-Cursor,
- Tombstones,
- Konfliktmaterialisierung,
- Cross-OS-Mehrgerätetests.

Exit-Kriterien: `M2-AC-001` bis `M2-AC-004`.

## 26.3 Meilenstein 3 – Erinnerungen

Technische Ergebnisse:

- Reminder-Frontmatter-Schema,
- plattformunabhängige Kalenderengine,
- Instanzzustandsautomat,
- lokale Scheduler,
- native Benachrichtigungsadapter,
- Presence und Zustell-Leases,
- Konflikttests für Offline-Aktionen.

Exit-Kriterien: `M3-AC-001` bis `M3-AC-005`.

## 26.4 Meilenstein 4 – Öffentliche Betriebsreife

Technische Ergebnisse:

- vollständige Recovery- und Löschabläufe,
- Rate Limits und Auditierung,
- Backup-/Restore-Automation,
- Telemetrie und Monitoring,
- CI-Artefakte für drei Plattformen,
- signiertes Release-Manifest,
- Versionsendpunkt und Mindestversion,
- Betriebs- und Datenschutzdokumentation.

Exit-Kriterien: `M4-AC-001` bis `M4-AC-005`.

## 27. Trade-offs und verworfene Alternativen

## 27.1 PostgreSQL statt SQLite

**Vorteile:** bessere Parallelität, etablierte Serveroperationen, spätere Skalierung.

**Entscheidung:** Für die erste Zielgröße und einen einzelnen Backendprozess wurde SQLite gewählt. Eine spätere Migration bleibt möglich.

## 27.2 Objektspeicher statt lokalem Blobvolume

**Vorteile:** integrierte Dauerhaftigkeit und Skalierung.

**Entscheidung:** zusätzlicher Dienst und höhere Backup-/Konsistenzkomplexität sind für V1 nicht gerechtfertigt.

## 27.3 Alles in SQLite

**Vorteile:** eine transaktionale Sicherungsgrenze.

**Nachteile:** große Versionsdatenbank, schlechtere Blobprüfung und mögliche Write-Amplification.

**Entscheidung:** relationale Metadaten in SQLite, Inhalte als unveränderliche Hash-Blobs.

## 27.4 Automatischer Drei-Wege-Merge

**Vorteile:** weniger sichtbare Konflikte.

**Entscheidung:** V1 bevorzugt nachvollziehbaren Erhalt beider Fassungen. Automatischer Merge ist ein bewusstes Nicht-Ziel.

## 27.5 Sidecar-Dateien

**Vorteile:** reines Markdown ohne Frontmatter.

**Nachteile:** Sidecars können bei externer Bearbeitung getrennt oder verwaist werden.

**Entscheidung:** fachliche Identität und Reminder im Frontmatter; technische Daten im zentralen versteckten Index.

## 27.6 Ende-zu-Ende-Verschlüsselung

**Vorteile:** Server kann Inhalte nicht lesen.

**Nachteile:** deutlich komplexere Geräteaufnahme, Recovery, Schlüsselverwaltung und Fehlersuche.

**Entscheidung:** kein E2E in V1; TLS und verschlüsselte Servermedien sind Pflicht.

## 27.7 Hintergrunddienst für Erinnerungen

**Vorteile:** Benachrichtigungen bei geschlossenem Fenster/Prozess.

**Entscheidung:** V1 benachrichtigt nur bei geöffnetem App-Prozess. Der eingeschränkte Zustellumfang muss klar kommuniziert werden.

## 28. Risiken und Gegenmaßnahmen

### R-001 – Watcher-Unterschiede

**Risiko:** Eventverluste und unterschiedliche Editor-Speichermuster.

**Gegenmaßnahmen:** Watcher nur als Hinweis, vollständiger Reconcile, Cross-OS-Testkorpus, keine zeitbasierten Ignore-Hacks.

### R-002 – Datenverlust durch Sync-Fehler

**Risiko:** Offline- und Strukturkonflikte sind komplex.

**Gegenmaßnahmen:** vollständige Versionen, Konfliktkopien, modellbasierte Tests, Fehler-Injektion und Idempotenz.

### R-003 – Reminder-Kalenderfehler

**Risiko:** DST, Monatsenden und widersprüchliche Aktionen.

**Gegenmaßnahmen:** feste TZID, gemeinsame Engine/Testvektoren, expliziter Zustandsautomat, keine impliziten OS-Regeln.

### R-004 – SQLite-/Single-Server-Grenzen

**Risiko:** Ausfallzeit und Write-Contention.

**Gegenmaßnahmen:** ein Schreibprozess, kurze Transaktionen, Monitoring, tägliche externe Backups und dokumentierter Restore.

### R-005 – Speicherwachstum

**Risiko:** unveränderliche Versionen und lange Tombstone-Aufbewahrung.

**Gegenmaßnahmen:** Metriken, Aufbewahrungsregeln, sichere verzögerte GC und Kapazitätsalarme.

### R-006 – Unsignierte Builds

**Risiko:** Betriebssystemwarnungen, Manipulation und langsame Sicherheitsupdates.

**Gegenmaßnahmen:** SHA-256, signiertes Manifest, TLS-Versionsendpunkt, Mindestversion, klare Installationshinweise.

### R-007 – Geringe Testabdeckung

**Risiko:** nur ein reales System je Plattform und zunächst eine Testperson.

**Gegenmaßnahmen:** Automatisierung, simulierte Mehrgeräte, Langzeittests, Fehler-Injektion und späterer expliziter Pilotentscheidungspunkt.

### R-008 – Offene Registrierung

**Risiko:** Botkonten, E-Mail-Missbrauch und Credential-Angriffe.

**Gegenmaßnahmen:** Verifikation, Rate Limits, Enumeration-Schutz, Monitoring und begrenzte Ressourcen pro Konto.

## 29. Bewusst vertagte technische Entscheidungen

Vor den jeweiligen Meilensteinen sind ADRs oder ergänzende Spezifikationen erforderlich für:

1. exaktes Frontmatter-Schema und YAML-Bibliothek,
2. genaue portable Längenlimits und Vergleichsfunktion,
3. Ordner-Reconcile nach Indexverlust,
4. Sync-API, Cursorbereich und Tombstone-Aufbewahrung,
5. Reminder-Instanzautomat und RRULE-Teilumfang,
6. DST-Sprungregeln und TZDB-Versionierung,
7. Online-Lease-Zeitfenster und Geräteauswahl,
8. Konfliktjoin widersprüchlicher Reminder-Aktionen,
9. Tokenformat, Rotation und lokale Secret-Service-Fallbacks,
10. Rate Limits und E-Mail-Anbieter,
11. Audit- und Diagnosedatenaufbewahrung,
12. Backupgenerationen, Restore-Frequenz und GC-Schutzfrist,
13. Release-Signaturalgorithmus, Schlüsselrotation und Sperrprozess,
14. genaue Installationsformate pro Plattform.

## 30. Architektur-Invarianten

Folgende Regeln dürfen ohne explizite Architekturänderung nicht verletzt werden:

1. Der lokale Markdown-Baum bleibt ohne Server les- und bearbeitbar.
2. Der technische Index ist nicht die einzige Quelle fachlicher Daten.
3. Keine konkurrierende Inhaltsfassung wird still verworfen.
4. Gerätezeitstempel entscheiden keine Konflikte.
5. Jeder serverseitig referenzierte Blob wurde vorher dauerhaft gespeichert und hashgeprüft.
6. Jeder Backup-Snapshot ist nur gültig, wenn alle referenzierten Blobs vorhanden und geprüft sind.
7. Objektzugriffe sind immer an den authentifizierten Benutzer gebunden.
8. Diagnose enthält keine Inhalte, Namen, Pfade, Frontmatter-Werte oder Reminder-Texte.
9. Mindestversionssperren blockieren keine lokale Offline-Bearbeitung.
10. Ein einzelner Konflikt blockiert nicht den Sync unbeteiligter Objekte.

## 31. Nachverfolgbarkeit

| Bereich         | Primäre PRD-Anforderungen | Designabschnitte |
| --------------- | ------------------------- | ---------------- |
| Lokale Dateien  | `DATA-001`–`DATA-010`     | 5–7              |
| Portable Namen  | `NAME-001`–`NAME-006`     | 8                |
| Synchronisation | `SYNC-001`–`SYNC-013`     | 9–14             |
| Erinnerungen    | `REM-001`–`REM-015`       | 15–16            |
| Konten          | `ACC-001`–`ACC-007`       | 10, 17, 19       |
| Sicherheit      | `SEC-001`–`SEC-009`       | 17–20            |
| Betrieb/Backup  | `OPS-001`–`OPS-008`       | 21–23            |
| Telemetrie      | `TEL-001`–`TEL-006`       | 22               |
| Releases        | `REL-001`–`REL-008`       | 24               |
| Akzeptanz       | `M1-AC-*`–`M4-AC-*`       | 25–26            |

## 32. Nächste technische Schritte

1. Meilenstein-1-ADR für lokale Identität, Index und Reconcile erstellen.
2. Portable Namensregeln als ausführbare Go-Spezifikation mit Testvektoren implementieren.
3. Frontmatter-Schema v1 für Notiz-ID definieren.
4. Wails-Spikes für Watcher, atomare Writes, Benachrichtigungen und sichere Tokenablage auf allen drei Plattformen durchführen.
5. Modellbasierten lokalen Dateibaum-Testharness erstellen.
6. Erst nach erfolgreichem lokalen Datenkern das Sync-Protokoll und serverseitige Schema finalisieren.
