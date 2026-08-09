# Changelog

Alle wesentlichen Änderungen an Remember werden in dieser Datei dokumentiert. Das Projekt hat noch kein veröffentlichtes Release; Einträge bleiben bis zur ersten Version unter **Unreleased**.

Das Format orientiert sich an [Keep a Changelog](https://keepachangelog.com/de/1.1.0/).

## [Unreleased]

### Added

- Wails-Desktop-Client mit Go, Svelte 5, TypeScript und Vite.
- Lokaler Markdown-Datenkern mit YAML-Frontmatter v1, UUIDv7, atomaren Schreibvorgängen und optimistischen Revisionen.
- SQLite-Index, Scanner, Reconcile, Move-Erkennung und rekursiver Dateisystem-Watcher.
- Notiz- und Ordnerverwaltung einschließlich Tags, Markdown-Vorschau, recoverbarem Löschen und Konfliktschutz.
- Eingebaute Remember-, Nord-, Dracula-, Solarized- und Catppuccin-Themes.
- Modularer Go-Server mit SQLite/WAL, checksum-geprüften Migrationen, Health-/Readiness-Probes und sicherem Docker-Image.
- Interner Identity-Core mit E-Mail-Kanonisierung, Argon2id, Verifikationstokens und Enumeration-Schutz.
- Interner mandantengebundener Sync-Core mit idempotenten Operationen, Versionen, Konflikten, Tombstones, Cursor-Pull und server-provisioniertem Konfliktbereich.
- Internes unveränderliches SHA-256-Blob-Repository mit 8-MiB-Grenze, Staging-Recovery und Startup-Audit.
- Sessions-/Devices-Core mit opaken Access-Tokens, rotierenden Refresh-Tokens, Replay-Erkennung und sofortigem Widerruf.
- Begrenzter Auth-HTTP-Transport für Login, Refresh, Logout sowie Sitzungs- und Geräteverwaltung.
- Authentifizierter Blob-HTTP-Transport mit strikt mandantengebundenem PUT/GET, 8-MiB-Requestgrenze und konfigurierbarer logischer Benutzerquota.
- Authentifizierter Sync-HTTP-Transport für idempotente Einzeloperationen, paginierten Cursor-Pull und wiederholbare kanonische Konfliktzustände.
- Lokaler Index v21 mit sequenziellen Migrationen, unveränderlicher Outbox, Sync-Baselines, Cursor sowie persistenten Folder-Restore-, Folder-Move-Revert-, lokalen Folder-Intent-, Konflikt-, Rebase-, No-op- und generationsgebundenen Blob-Cleanup-Journalen.
- Exaktes, fsync-gesichertes Outbox-Blob-Staging sowie atomare Reconcile-/Outbox-Erfassung.
- Crash-resumierbarer Remote-Apply für Notiz-CRUD sowie identitätsgebundene Folder-Create/-Move/-Delete-Operationen ohne permanente Marker.
- Strikter Client-HTTP-Transport und manueller Vordergrund-Sync mit crash-sicherer Wiederholung mehrdeutiger Operations-Submits.
- Expliziter Bootstrap für bestehende v1-Roots und UUIDv4-kompatible Sync-Objektidentitäten.
- Mindest-Rate-Limits, strikte Requestgrenzen und begrenzte Argon2-Parallelität für öffentliche Login-/Refresh-Routen.
- Automatisierter Mehrgeräte-Konvergenztest über den vollständigen produktionsnahen Login-, Blob- und Sync-HTTP-Stack einschließlich Serverneustart, kaltem History-Bootstrap, unterbrochener Pull-Seiten-Wiederaufnahme sowie Note-/Folder-Move/-Delete.
- PRD, technisches Design, Architekturentscheidungen und manuelle Testpläne.

### Security

- Descriptor-verankerte lokale Dateioperationen auf Darwin/Linux mit Schutz vor Symlink-Rennen.
- Bereinigte Markdown-Vorschau ohne aktive Inhalte, gefährliche Links oder Bilder.
- Domain-separierte Hashspeicherung für Verifikations-, Access- und Refresh-Tokens.
- Strikte Tenant-Bindung für Sync, Blobs, Geräte und Sitzungen.
- Keine Geheimnisse, dynamischen IDs oder Requestinhalte in HTTP-Logs.

### Known limitations

- Noch keine öffentliche Registrierung, Recovery oder Kontolöschung.
- Crash-sichere, sichtbare und synchronisierte Konfliktkopien für konkurrierende Notiz-Updates und lokale Edits gegen kanonische Remote-Deletes einschließlich descriptor-gebundener technischer Bereinigung.
- Edit-vs-Delete-Apply überspringt authentifizierte kanonische Zwischen-Updates/-Moves, erhält die lokalen Bytes im recoverable Trash und nimmt Abstürze über Pull-Seitengrenzen wieder auf.
- Lokale Deletes gegen Remote-Edits retten zuerst den authentifizierten kanonischen Blob als synchronisierte Konfliktkopie und enqueueen den Tombstone anschließend atomar auf der neuen Revision.
- Konkurrierende Note-Creates am selben portablen Pfad evakuieren die verlierenden Bytes crash-sicher und materialisieren sie erst nach baseline-gebundener Anwendung des Remote-Gewinners.
- Note-Move-Pfadkollisionen stellen den authentifizierten kanonischen Quellzustand wieder her und retten die lokal verschobenen sowie abhängig bearbeiteten Bytes als synchronisierte Konfliktkopie.
- Note-Updates und -Moves gegen `object_missing` evakuieren verwaiste Quellbytes samt abhängiger Edits mit crash-sicherer Delete-Unterdrückung und synchronisieren sie unter neuer UUID.
- Deletes gegen bereits fehlende Remote-Objekte werden ohne künstlichen Tombstone dauerhaft als `already_deleted` aufgelöst.
- Nach sichtbarer Konfliktveröffentlichung werden vollständige technische Staging- und Evakuierungsbytes descriptor-gebunden getruncated; nur leere crash-idempotente Sentinels bleiben zurück.
- Finale content-addressed Outbox-Blobs werden nur ohne aktive Replay-/Konfliktreferenz und pro Hash-Wiederverwendung in einer eigenen Sequenzgeneration bereinigt.
- Lokal gelöschte, serverseitig nichtleere Ordner werden nonce-/inode-gebunden restauriert, bevor ihre Remote-Kinder gepullt werden.
- Note-Create/-Move unter einem bereits remote gelöschten Parent retten lokale Bytes crash-sicher als sichtbare synchronisierte Konfliktkopie.
- Leere Folder-Creates mit Pfadkollision oder inzwischen gelöschtem Zielparent werden unter neuer UUID sichtbar und synchronisiert in `_Konflikte/Wiederhergestellt` gerettet; nichtleere Varianten bleiben fail-closed.
- Folder-Moves in einen gelöschten Parent sind mit Absturz nach Inode-Revert und anschließend synchronisiertem abhängigem Kind-Edit verifiziert.
- Folder-Moves gegen belegte Pfade, gelöschte Remote-Parents oder eine inzwischen divergierte zyklische Ancestry werden auf den authentifizierten kanonischen Pfad inode-gebunden zurückgesetzt; äquivalente konkurrierende Moves werden ohne Dateisystemänderung aufgelöst und abhängige Kindänderungen bleiben sendbar. Divergente Move-Ziele bleiben fail-closed.
- Note-/Folder-`type_mismatch` wird in beiden Richtungen explizit vor Pull und Apply blockiert; lokale Bytes, Inodes und Unterbäume bleiben unverändert und der kanonische Konfliktzustand ist replay-stabil.
- Konkurrierende Note-Moves und -Deletes konvergieren in beiden Reihenfolgen: Der Tombstone bleibt wirksam, die Move-Fassung wird unter neuer UUID sichtbar gerettet und technische Evakuierungsbytes werden erst danach hashgebunden bereinigt.
- Stale Doppel-Deletes für Notes und leere Folder werden nach exakt typ- und revisionsgebundenem kanonischem Tombstone idempotent als `already_deleted` aufgelöst.
- Create-`object_exists` ist für Notes und nichtleere Folder-Unterbäume explizit fail-closed verifiziert; lokale und kanonische Fassungen bleiben bei Replay unverändert.
- Divergente konkurrierende Root-Note-Moves retten die verlierende Fassung samt abhängigem Edit unter neuer UUID; äquivalente Root-Ziele werden als streng gebundener No-op aufgelöst und abhängige Edits bleiben sendbar. Nicht-root-basierte Varianten bleiben bis zur sicheren Ancestry-Auflösung fail-closed.
- Nichtleere Folder-Create-Pfad-/Parent-Konflikte werden für streng manifestierte direkte, nie versuchte Note-Creates rekursionsfrei gerettet; Note-UUIDs und Bytes bleiben erhalten, Nested Folder und spätere Edits bleiben fail-closed.
- Authentifizierte Drei-Geräte-HTTP-Konvergenz deckt beide Note-Move/Delete-Reihenfolgen und die Direct-Note-Folder-Recovery einschließlich kaltem Bootstrap ab.
- Leere lokale Folder-Moves gegen kanonische Remote-Deletes werden unter neuer UUID sichtbar gerettet; Original-ID und exakte Tombstone-Revision bleiben wirksam. Nichtleere und später weiter mutierte Varianten bleiben fail-closed.
- Eigene akzeptierte Folder-Move/-Delete-Echos werden ausschließlich über atomar persistierte Quellpfad-/Device-/Inode-Intents als lokal ausgeführt bestätigt.
- Mehrere Note-/Folder-Zustände derselben Pull-Seite verwenden pfadgenaue Zwischenzustände; Create→Delete-Publikationsmarker werden crash-resumierbar vom passenden Delete konsumiert.
- Ein Netzwerkabbruch zwischen Pull-Seiten setzt nach Clientneustart nachweislich am dauerhaft bestätigten `NextCursor` statt bei Cursor null fort.
- Hintergrund-Scheduler und OS-Schlüsselspeicher fehlen weiterhin.
- Noch keine sichere Clientablage für Refresh-Tokens.
- Reminder sowie breitere Zwei-Geräte-Konvergenz für die verbleibenden Folder-/Strukturkonflikte sind noch nicht implementiert.
- Windows- und Linux-Desktop-Builds wurden noch nicht real auf Zielgeräten geprüft.
- Builds sind unsigniert und besitzen noch keinen automatischen Updater.
- TLS muss durch einen vorgeschalteten Reverse Proxy terminiert werden.
