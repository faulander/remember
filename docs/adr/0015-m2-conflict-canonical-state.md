# ADR 0015: Authentifizierter kanonischer Zustand bei Sync-Konflikten

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Ein persistierter Konfliktcode allein reicht nicht zur verlustfreien Client-Materialisierung. Insbesondere erzeugt eine abgelehnte Operation keinen neuen Change-Log-Eintrag; ein späterer Pull muss daher nicht den Zustand liefern, gegen den der Server die Operation abgelehnt hat. Ohne authentifizierten und wiederholbaren kanonischen Zustand kann der Client weder die Serverfassung sicher laden noch Delete-Konflikte korrekt neu basieren.

## Entscheidung

Der Server persistiert mit jeder konfliktbehafteten Operation den zum Konfliktzeitpunkt gelesenen kanonischen Objektzustand, sofern das betroffene Objekt existiert: Typ, Revision, Parent-ID, Name, Blob-Hash und Tombstone-Status. Idempotente Wiederholungen derselben Operations-ID liefern exakt diesen gespeicherten Zustand und nicht einen möglicherweise später fortgeschrittenen Objektzustand.

`POST /v1/sync/operations` ergänzt das immer vorhandene Feld `canonical`. Bei akzeptierten Operationen ist es `null`; bei Konflikten ist es entweder der gespeicherte Zustand oder `null`, wenn das vorgeschlagene Objekt nicht existierte. Der Client validiert die verschachtelte Antwort strikt, einschließlich UUID-, Revisions-, Namens-, Typ- und SHA-256-Grenzen, und persistiert vorhandene Zustände atomar zusammen mit dem finalen Outbox-Konflikt.

Dieser Schnitt materialisiert noch keine sichtbaren Konfliktkopien. Der erste darauf aufbauende Apply-Schnitt wird auf `base_revision_mismatch` für Notizen begrenzt. Reservierte synchronisierte Konfliktordner und Delete-Rebase benötigen eigene, explizite Protokollentscheidungen.

## Folgen

Konfliktentscheidungen können crash-sicher auf dem Zustand beruhen, der tatsächlich zur Ablehnung führte. Zusätzliche nullable Spalten in `sync_operations` erhöhen die Metadatenmenge geringfügig; Note-Inhalte bleiben weiterhin ausschließlich blob-referenziert. Konflikte ohne existierendes Zielobjekt benötigen weiterhin objekttyp-spezifische Auflösung.
