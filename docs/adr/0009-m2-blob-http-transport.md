# ADR 0009: Authentifizierter M2-Blob-HTTP-Transport

- Status: Angenommen
- Datum: 2026-08-06

## Kontext

Der Auth-Transport kann einen mandantengebundenen Principal ableiten, und das interne Blob-Repository speichert unveränderliche SHA-256-Inhalte. Für nachfolgende Sync-Operationen müssen Clients Blob-Bytes übertragen können, ohne eine globale Existenzabfrage oder unbeschränkten Datenträgerverbrauch zu ermöglichen.

## Entscheidung

### Routen und Autorisierung

- `PUT /v1/blobs/{sha256}` veröffentlicht oder dedupliziert einen Blob für den authentifizierten Benutzer.
- `GET /v1/blobs/{sha256}` liest ausschließlich einen bereits für diesen Benutzer berechtigten Blob.
- `{sha256}` ist exakt 64 Zeichen lang und kanonisch kleingeschriebenes Hexadezimal.
- Der Benutzer wird ausschließlich aus einem gültigen Bearer-Access-Token abgeleitet. `user_id` wird weder im Pfad noch in Body oder Query akzeptiert.
- Authentifizierung erfolgt vor Hash- und Blob-Header-Validierung. Nicht vorhandene, nicht berechtigte und fremde Blobs liefern identisch `404 blob_not_found`.

PUT verlangt genau `Content-Type: application/octet-stream`, kein `Content-Encoding` und eine bekannte `Content-Length` zwischen 0 und 8 MiB. Die tatsächliche Länge muss exakt übereinstimmen. Höchstens vier Uploads werden gleichzeitig verarbeitet. Erfolgreiches PUT liefert `200` mit kanonischem Hash und Größe; idempotente Wiederholung hat denselben Vertrag.

GET liefert `application/octet-stream`, exakte `Content-Length` und `Cache-Control: no-store`. Range-, Content-Range- und bedingte Requests werden nicht unterstützt und als `invalid_request` abgelehnt. Logs enthalten nur das Routentemplate `/v1/blobs/{hash}`.

### Logische Benutzerquota

`REMEMBER_USER_BLOB_QUOTA_BYTES` konfiguriert eine positive kanonische Dezimalzahl. Standard sind 1 GiB, maximal zulässig ist 1 TiB.

Die Quota zählt die Summe der Größen aller eindeutigen `user_content_blobs`-Berechtigungen eines Benutzers. Globale physische Deduplizierung reduziert diese logische Tenant-Nutzung nicht. Eine bereits vorhandene Berechtigung ist idempotent und wird nicht erneut gezählt. Da unveränderliche Historie noch nicht bereinigt wird, zählen auch ältere berechtigte Inhaltsversionen.

Nach Staging, Hashprüfung und `fsync` beginnt `Put` eine SQLite-Transaktion. Eine erste Schreiboperation auf dem aktiven Benutzer serialisiert Quota-Prüfungen auch über mehrere Repository-Instanzen. Bei einer neuen Berechtigung wird die Nutzung geprüft; eine Überschreitung endet vor finaler Veröffentlichung. Erst danach wird atomar veröffentlicht beziehungsweise ein vorhandenes Ziel vollständig verifiziert, der Blob registriert und die Tenant-Berechtigung committed. Damit bleiben Bytes weiterhin vor jeder SQLite-Referenz dauerhaft vorhanden. Ein Datenbankfehler nach Veröffentlichung kann wie bisher einen auditierbaren Orphan erzeugen; Quota-Ablehnung, Hashabweichung und Größenüberschreitung dürfen das nicht.

Öffentliche Fehler sind begrenzt: Quota und zu große Inhalte liefern `413`, Hashabweichung `422`, unbekannte/fremde Blobs `404`, interne Integritäts- oder Speicherfehler `500` ohne Details.

## Explizit nicht enthalten

- Garbage Collection, Quota-Freigabe durch History-Pruning oder Reparatur,
- Range- und Conditional-GET, öffentliche Cachefähigkeit oder ETags,
- eine globale Blob-Existenzabfrage,
- resumierbare oder komprimierte Uploads,
- öffentliche Sync-Endpunkte,
- globale Datenträgerquota oder verteilte Quota-Koordination für mehrere Serverprozesse.

## Folgen

Ein einzelner Benutzer kann seine logisch berechtigten Inhalte nicht unbegrenzt vergrößern. Betreiber müssen die Quota passend zur Datenträgerkapazität und maximalen Kontenzahl wählen. Weil V1 genau einen Serverprozess vorsieht und SQLite die dauerhafte Serialisierung übernimmt, bleibt die Prüfung race-sicher; horizontale Skalierung erfordert eine erneute Entscheidung.
