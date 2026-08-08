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
- Lokaler Index v8 mit sequenziellen Migrationen, unveränderlicher Outbox, Sync-Baselines, Cursor sowie persistenten Folder-, Konflikt-, Rebase- und Cleanup-Journalen.
- Exaktes, fsync-gesichertes Outbox-Blob-Staging sowie atomare Reconcile-/Outbox-Erfassung.
- Crash-resumierbarer Remote-Apply für Notiz-CRUD sowie identitätsgebundene Folder-Create/-Move/-Delete-Operationen ohne permanente Marker.
- Strikter Client-HTTP-Transport und manueller Vordergrund-Sync mit crash-sicherer Wiederholung mehrdeutiger Operations-Submits.
- Expliziter Bootstrap für bestehende v1-Roots und UUIDv4-kompatible Sync-Objektidentitäten.
- Mindest-Rate-Limits, strikte Requestgrenzen und begrenzte Argon2-Parallelität für öffentliche Login-/Refresh-Routen.
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
- Note-Updates gegen `object_missing` evakuieren verwaiste Quellbytes mit crash-sicherer Delete-Unterdrückung und synchronisieren sie unter neuer UUID; weitere Konfliktklassen fehlen weiterhin.
- Hintergrund-Scheduler und OS-Schlüsselspeicher fehlen weiterhin.
- Noch keine sichere Clientablage für Refresh-Tokens.
- Reminder und Zwei-Geräte-End-to-End-Konvergenz sind noch nicht implementiert.
- Windows- und Linux-Desktop-Builds wurden noch nicht real auf Zielgeräten geprüft.
- Builds sind unsigniert und besitzen noch keinen automatischen Updater.
- TLS muss durch einen vorgeschalteten Reverse Proxy terminiert werden.
