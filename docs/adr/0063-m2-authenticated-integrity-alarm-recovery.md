# ADR 0063: Authentifizierte Integritätsalarm- und Recovery-Abnahme

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

ADR 0062 prüfte Persistenz und Sichtbarkeit mit isolierten Transport- und Apply-Tests. Eine zusammengesetzte Abnahme über echte Auth-, Sync- und Blob-Routen einschließlich Restart und erfolgreicher Fortsetzung fehlte.

## Entscheidung und Nachweis

`TestAuthenticatedBlobIntegrityAlarmsResumeAfterRestart` verwendet den realen Integrationserver und echte Logins für A und B. Ein testlokaler Reverse Proxy verändert ausschließlich Bs Blob-GET-Antwort:

1. `blob_not_found` erzeugt einen plan-/cursor-/objekt-/hashgebundenen `missing_blob`-Alarm ohne Dateipublikation oder Confirmed-Fortschritt.
2. Nach Client-Neustart bleibt derselbe Plan aktiv; derselbe Fehler erhöht dedupliziert den Vorkommenszähler.
3. Eine erfolgreiche Antwort mit falschen Bytes wird vom HTTP-Transport als typisierter `ErrBlobHashMismatch` erkannt und als separater `hash_mismatch`-Alarm gespeichert.
4. Nach Freigabe der unveränderten echten Serverantwort setzt derselbe pending Plan fort, publiziert exakte Bytes und UUID einmalig und bringt Confirmed- und Downloaded-Cursor wieder zusammen.

Der Produktionsserver und seine Blob-Prüfung werden für die Fehlerinjektion nicht abgeschwächt.

## Folgen

M2-AC-004 ist im Clientprodukt automatisiert erfüllt: fehlende und hashinkonsistente Inhalte werden über reale Grenzen erkannt, dauerhaft alarmiert, nach Restart bewahrt und nach Wiederherstellung fortgesetzt. Eine externe Betriebsmetrik ist kein zusätzliches M2-Kriterium.
