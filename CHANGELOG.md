# Changelog

Alle wesentlichen Änderungen an Remember werden in dieser Datei dokumentiert. Das Projekt hat noch kein veröffentlichtes Release; Einträge bleiben bis zur ersten Version unter **Unreleased**.

Das Format orientiert sich an [Keep a Changelog](https://keepachangelog.com/de/1.1.0/).

## [Unreleased]

### Added

- Der Wails-Desktop-Client kann eine Serversitzung per Login öffnen, Access-Tokens über rotierende Refresh-Tokens erneuern, einen begrenzten Vordergrund-Sync auslösen und die Sitzung serverseitig widerrufen. Refresh-Tokens werden ohne Klartext-Fallback ausschließlich in macOS Keychain, Linux Secret Service beziehungsweise Windows Credential Manager gespeichert, bei Wiederaufnahme vor Verwendung rotiert und nach jeder Rotation vor Tokenfreigabe erneut versiegelt; Passwort und Access-Token bleiben im Arbeitsspeicher und Tokens werden nie an das Frontend zurückgegeben. Die Sitzungsverwaltung gruppiert alle Sitzungen nach Gerät, erlaubt das Umbenennen des aktuellen Geräts und widerruft andere Sitzungen einzeln oder ein anderes Gerät samt aller seiner Sitzungen.
- ADR 0075 und Protokoll v3 bewahren aktive direkte Notes bei Folder Delete/Remote-Move unter derselben Note-UUID und mit exaktem Blob; Servermigration 008 und Client-Schema v35 versiegeln Recovery-Root-, Folder-Clone- und Note-Move-Deskriptoren, superseden nur exakt gebundene lokale Delete-Intents und promoten vorbereitete v2-Versuche ausschließlich nach authentifizierter expliziter Ablehnung dauerhaft mit neuer Operations-ID.
- ADR 0074 erweitert Preserve-and-delete transaktional auf aktuelle direkte leere Child-Folder; versionierter Replay-Hash, bestätigte Cursor-Grenze, zusammenhängende Spanne und Clone-Mapping bleiben dauerhaft gebunden.
- Client-Schema v34 bindet die leere Preserve-and-Delete-Historie exakt und schließt nur die konkrete aufgelöste Konfliktoperation aus ungelösten Intents aus.
- Servermigration 007 ergänzt v2-Metadaten und unveränderliche Child-Clone-Zuordnungen bei kompatiblem v1-Replay.
- ADR 0073 nimmt die leere Folder Delete/Remote-Move-Resolution mit A/B und kaltem C über echte Auth-/Sync-Routen ab.
- Client-Schema v32 und ADR 0072 persistieren die exakt konfliktgebundene leere Preserve-and-Delete-Resolution und ergänzen den strikten HTTP-Client.
- ADR 0071 exponiert die leere Preserve-and-Delete-Resolution über einen strikten tokengebundenen Sync-HTTP-Transport.
- Servermigration 006 und ADR 0070 implementieren den actor-gebundenen atomaren Preserve-and-Delete-Core zunächst für aktuell leere Folder mit exaktem Replay und normalen Change-Log-Zeilen.
- ADR 0069 definiert die atomare servergestützte Preserve-and-Delete-Operation für Folder Delete gegen Remote-Move mit vollständigem Subtree-Clone, neuen UUIDs und child-first Tombstones.
- Schema v31 erweitert divergente root-level Folder-Move-Recovery auf exakt manifestierte direkte lokal erstellte Notes mit linearen nie versuchten Create→Update-Ketten; A/B, B-Restart und kalter C sind über echte Auth-/Blob-/Sync-Routen abgenommen. Branches, attempted, Nested und unbekannte Dateien bleiben fail-closed.
- Schema v30 implementiert die crash-fortsetzbare Recovery leerer divergenter root-level Folder-Moves mit durablem Canonical-Publish, erhaltener Verlierer-Inode und neuer Recovery-UUID; A/B, B-Restart und kalter C sind über echte Auth-/Sync-Routen abgenommen.
- ADR 0064 legt für divergente Folder Move/Move den kanonischen Serverpfad mit verlustfreier Recovery des lokalen Verlierer-Subtrees fest; Folder Delete gegen Remote-Move bleibt bis zu einer atomaren servergestützten Preserve-and-Delete-Lösung protokoll-blockiert.
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
- Lokaler Index v29 mit sequenziellen Migrationen, unveränderlicher Outbox, Sync-Baselines, Cursor sowie persistenten Folder-Restore-, Folder-Move-Revert-, lokalen Folder-Intent-, Konflikt-, Rebase-, No-op- und generationsgebundenen Blob-Cleanup-Journalen.
- Dauerhafte Sync-Inbox mit atomarer Pull-Seitenaufnahme, exaktem Replay-Schutz, monotonem Apply-Zustand und getrenntem Download-/Confirmed-Cursor; `SyncOnce` spiegelt den bestehenden cursor-geordneten Apply-Pfad crash-fortsetzbar. Schema v29 persistiert fehlende und hashinkonsistente Remote-Blobs plan-/cursor-/objektgebunden als sichtbare, deduplizierte Integritätsalarme; eine authentifizierte Proxy-Fehlerinjektion prüft Missing, falsche Bytes, Restart, Sichtbarkeit und anschließende Fortsetzung desselben Plans; Schema v28 schließt ungelöste lokale Objekt-Intents bereits vor dem begrenzten Kandidatenscan aus und schützt Planerzeugung/Retry mit derselben zentralen SQL-Sicht; Schema v27 ergänzte einen explizit bewachten Retry abgebrochener Planlinks; Schema v26 ergänzte unveränderlich gebundene Einzelpläne und einen begrenzten out-of-order Scheduler für unabhängige lineare Root-Note-Update/-Delete-Ketten bei weiterhin ungelöstem Outbox-Konflikt; eine authentifizierte A/B-/Cold-C-Abnahme belegt dies hinter einem bewusst ungelösten divergenten Folder-Move, während der bestätigte Präfix lückenlos bleibt. Strukturänderungen und vollständige Isolation für `SYNC-012` bleiben offen.
- Exaktes, fsync-gesichertes Outbox-Blob-Staging sowie atomare Reconcile-/Outbox-Erfassung.
- Descriptor-rekursive, nicht mutierende Subtree-Verifikation auf Darwin/Linux mit exaktem Manifest, Folder-Inode-Bindung, `O_NOFOLLOW` und Datei-SHA-256 als Grundlage für spätere Nested-Folder-Recovery.
- Crash-resumierbarer Remote-Apply für Notiz-CRUD sowie identitätsgebundene Folder-Create/-Move/-Delete-Operationen ohne permanente Marker.
- Strikter Client-HTTP-Transport und manueller Vordergrund-Sync mit crash-sicherer Wiederholung mehrdeutiger Operations-Submits.
- Expliziter Bootstrap für bestehende v1-Roots und UUIDv4-kompatible Sync-Objektidentitäten.
- Mindest-Rate-Limits, strikte Requestgrenzen und begrenzte Argon2-Parallelität für öffentliche Login-/Refresh-Routen.
- Automatisierter Mehrgeräte-Konvergenztest über den vollständigen produktionsnahen Login-, Blob- und Sync-HTTP-Stack einschließlich Serverneustart, kaltem History-Bootstrap, unterbrochener Pull-Seiten-Wiederaufnahme sowie Note-/Folder-Move/-Delete.
- PRD, technisches Design, Architekturentscheidungen, manuelle Testpläne und ein expliziter M2-Konfliktmatrix-Audit mit konvergierenden, fail-closed und protokoll-blockierten Zellen.

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
- Divergente konkurrierende Root-Note-Moves und descriptor-verifizierte Nicht-Root-Moves innerhalb derselben exakten Parent-ID retten die verlierende Fassung samt abhängigem Edit unter neuer UUID; äquivalente Root- und descriptor-verifizierte Nicht-Root-Ziele werden als streng gebundener No-op aufgelöst und abhängige Edits bleiben sendbar. Nicht verifizierbare Ancestry-Varianten bleiben fail-closed.
- Nichtleere Folder-Create-Pfad-/Parent-Konflikte werden für streng manifestierte direkte Note-Creates mit ausschließlich linearer, nie versuchter Create→Update-Historie rekursionsfrei gerettet; Note-UUIDs und finale Bytes bleiben erhalten, Nested Folder und nichtlineare oder versuchte Historien bleiben fail-closed.
- Authentifizierte Drei-Geräte-HTTP-Konvergenz deckt beide Note-Move/Delete-Reihenfolgen, Direct-Note-Folder-Recovery mit linearen Updates, leere und direct-note-haltige Folder-Moves gegen Remote-Delete sowie äquivalente und divergente Same-Parent-Note-Moves einschließlich kaltem Bootstrap ab.
- Leere lokale Folder-Moves sowie streng manifestierte direkte Note-Subtrees mit linearer, nie versuchter Create→Update-Historie werden gegen kanonische Remote-Deletes unter neuer Root-UUID sichtbar gerettet; Note-UUIDs und finale Bytes bleiben erhalten, Original-ID und Tombstone wirksam. Nested, versuchte und nichtlineare Varianten bleiben fail-closed.
- Eigene akzeptierte Folder-Move/-Delete-Echos werden ausschließlich über atomar persistierte Quellpfad-/Device-/Inode-Intents als lokal ausgeführt bestätigt.
- Mehrere Note-/Folder-Zustände derselben Pull-Seite verwenden pfadgenaue Zwischenzustände; Create→Delete-Publikationsmarker werden crash-resumierbar vom passenden Delete konsumiert.
- Ein Netzwerkabbruch zwischen Pull-Seiten setzt nach Clientneustart nachweislich am dauerhaft bestätigten `NextCursor` statt bei Cursor null fort.
- Automatischer Hintergrund-Sync fehlt weiterhin; Synchronisierung wird im Desktop bewusst manuell ausgelöst.
- Reminder sowie breitere Zwei-Geräte-Konvergenz für die verbleibenden Folder-/Strukturkonflikte sind noch nicht implementiert.
- Windows- und Linux-Desktop-Builds wurden noch nicht real auf Zielgeräten geprüft.
- Builds sind unsigniert und besitzen noch keinen automatischen Updater.
- TLS muss durch einen vorgeschalteten Reverse Proxy terminiert werden.
