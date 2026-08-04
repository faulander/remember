# ADR 0006: Interner M2-Blob-Speicher

- **Status:** Akzeptiert
- **Datum:** 2026-08-03
- **Bezug:** technische Grundlage für `SYNC-002`, `SYNC-013` und `M2-AC-004`

## Scope

Dieser Schnitt implementiert den internen, unveränderlichen SHA-256-Blob-Speicher. HTTP-Upload, Sessions, Garbage Collection, Reparatur und Backup folgen getrennt.

## Grenzen und Layout

Ein Blob ist maximal 8 MiB groß. Diese feste Grenze gilt im Stream-Reader und als SQLite-Constraint. Der Server berechnet SHA-256 selbst und vergleicht ihn mit dem erwarteten Hash.

```text
<blob-root>/sha256/ab/cd/<64 lowercase hex>
<staging-root>/.upload-<zufall>
```

Blob- und Staging-Root sind reale, verschiedene Verzeichnisse auf demselben Dateisystem, Modus `0700`. Blobs werden mit `0600` erstellt; abweichende Modi schlagen bei Lesen und Audit fehl. Auf Darwin/Linux sind Root, Elternverzeichnisse, Veröffentlichung, Lesen und Entfernen descriptor-relativ und `O_NOFOLLOW`-gehärtet; neu angelegte Verzeichnisstufen werden vor einer Datenbankreferenz bis zu ihren Eltern synchronisiert. Der produktive Serverbetrieb ist gemäß Zielarchitektur Linux. Die Windows-Implementierung hält Entwicklungs- und Cross-Compile-Builds möglich, erhebt aber noch keine Produktionsgarantie für Verzeichnis-Durability oder Reparse-Point-Rennen.

## Write-before-reference

1. Maximal `8 MiB + 1` Bytes in eine zufällige Staging-Datei streamen und gleichzeitig hashen.
2. Größe und erwarteten Hash prüfen.
3. Datei `fsync`en und schließen.
4. Per exklusivem Hardlink unter dem Hashpfad publizieren; ein vorhandenes Ziel vollständig prüfen und niemals ersetzen.
5. Verzeichnisse synchronisieren und Staging-Datei entfernen.
6. Erst danach eine SQLite-Transaktion beginnen.
7. Aktiven Benutzer prüfen, globale Blobregistrierung einfügen beziehungsweise Größe/Verfügbarkeit validieren und die mandantenspezifische Berechtigung einfügen.

Ein DB-Fehler nach Veröffentlichung lässt bewusst einen später auditierbaren Orphan zurück. Deduplizierte und neue Writes liefern dasselbe Ergebnis ohne Existenzsignal.

## Mandantengrenze und Lesen

Nur `Repository.ForUser(userID)` bietet `Put` und `Get`. Es gibt keine globale Existenzabfrage. `Get` prüft zuerst aktives Konto, Berechtigung und Verfügbarkeitsregister. Fremde und unbekannte Hashes liefern denselben Fehler. Erst danach wird die Datei vollständig gelesen und gegen registrierte Größe und SHA-256 geprüft. Ein berechtigter fehlender oder korrupter Blob ist ein Integritätsfehler.

## Recovery und Audit

Startup-Recovery entfernt ausschließlich reguläre `.upload-*`-Dateien. Symlinks, Verzeichnisse und unerwartete Namen stoppen den Start als unsicher.

Der vollständige Audit:

- prüft jeden als verfügbar registrierten Blob auf Existenz, Größe und Hash,
- zählt Orphans,
- meldet malformed Pfade, unerwartete Objekte und Symlinks,
- repariert und löscht nichts.

Vor Readiness führt der Server Recovery und Audit aus. Fehlende, korrupte oder unsichere registrierte Daten verhindern Readiness. Orphans erzeugen ausschließlich eine aggregierte Warnung ohne Hash oder Pfad.

## Nicht enthalten

- HTTP-Upload/Download,
- Resumable Uploads,
- Garbage Collection oder Orphan-Löschung,
- automatische Reparatur,
- Backupgenerationen und Restore,
- öffentliche Integritätsdetails.
