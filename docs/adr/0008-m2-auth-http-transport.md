# ADR 0008: Begrenzter M2-Auth-HTTP-Transport

- Status: Angenommen
- Datum: 2026-08-05

## Kontext

Identity sowie der Sessions-/Devices-Core sind intern vorhanden. Für die folgenden M2-Transporte muss der Server eine kleine öffentliche Authentifizierungsfläche bereitstellen, ohne Tenant-IDs aus Requestdaten zu vertrauen oder bereits Registrierung, Recovery, Sync beziehungsweise Blob-Bytes öffentlich zu machen.

## Entscheidung

Der HTTP-Layer erhält ausschließlich ein typisiertes Sessions-Service-Interface. Die SQLite-Verbindung bleibt dort auf Readiness-Prüfungen beschränkt und steht Auth-Handlern nicht als Repository-Ersatz zur Verfügung. `app.Serve` konstruiert Production-Identity und Production-Sessions und injiziert Letzteren explizit.

### Routen

| Methode | Route | Zweck |
|---|---|---|
| `POST` | `/v1/auth/login` | E-Mail, Passwort und Gerätename gegen ein neues Gerät/eine neue Sitzung tauschen |
| `POST` | `/v1/auth/refresh` | Refresh-Token rotieren |
| `POST` | `/v1/auth/logout` | die durch Bearer-Access-Token bestimmte aktuelle Sitzung widerrufen |
| `GET` | `/v1/sessions` | eigene Sitzungen auflisten |
| `PATCH` | `/v1/devices/{uuidv7}` | eigenes Gerät umbenennen |
| `DELETE` | `/v1/devices/{uuidv7}` | eigenes Gerät und seine Sitzungen widerrufen |
| `DELETE` | `/v1/sessions/{uuidv7}` | eigene Sitzung widerrufen |

Dynamische IDs müssen kanonische UUIDv7 sein. Keine Route akzeptiert `user_id`; der Tenant entsteht ausschließlich aus dem Access-Token. Queryparameter werden auf dieser Fläche abgelehnt.

### Request- und Fehlergrenzen

- JSON wird nur mit exakt `Content-Type: application/json`, maximal 16 KiB, ohne unbekannte Felder und ohne nachlaufenden zweiten JSON-Wert akzeptiert.
- Bearer-Header müssen einzeln, exakt und begrenzt sein.
- Antworten tragen `Cache-Control: no-store`; Secrets erscheinen weder in Fehlermeldungen noch Logs.
- Öffentliche Fehlercodes sind begrenzt auf `invalid_request`, `invalid_credentials`, `invalid_session`, `not_found`, `rate_limited`, `method_not_allowed` und `internal_error`.
- Login unterscheidet nicht zwischen unbekannter, unverifizierter, inaktiver Identität oder falschem Passwort. Geschützte Routen und Refresh geben keine internen Sessionfehler preis.
- Requestlogs verwenden ausschließlich statische Routennamen beziehungsweise `{id}`-Templates und erfassen weder Body, Query noch dynamische Pfad-ID.

### Mindest-Abuse-Limits

V1 verwendet pro Prozess feste, concurrency-sichere Fensterlimits:

- Login: 10 Versuche pro normalisiertem E-Mail-Schlüssel je 15 Minuten sowie 100 Versuche global je 15 Minuten. Höchstens vier Passwortprüfungen laufen gleichzeitig; weitere Versuche erhalten unmittelbar `429`.
- Refresh: 30 Versuche pro Refresh-Token-Schlüssel je Minute sowie 300 Versuche global je Minute.
- Schlüssel werden domain-separiert mit SHA-256 gebildet; rohe E-Mail-Adressen und Tokens werden nicht im Limiter gehalten.
- Die Key-Maps sind auf jeweils 4096 Einträge begrenzt. Bei voller, noch nicht abgelaufener Map wird abgewiesen statt ein Schlüssel verdrängt und damit ein Bypass ermöglicht.
- `429` enthält `Retry-After`. Zeitrücksprünge beginnen ein neues begrenztes Fenster.

Diese Limits sind absichtliche Mindestwerte vor öffentlicher Exposition, keine vollständige adaptive Missbrauchserkennung. `X-Forwarded-For` und andere Client-IP-Header werden nicht vertraut oder ausgewertet. Die globalen Limits wirken daher unabhängig von Proxy-Topologie und ergänzen die geheimnisgebundenen Limits.

### Transportgrenze

Der Go-Prozess terminiert weiterhin kein TLS. Ein vorgeschalteter, korrekt betriebener TLS-Reverse-Proxy ist zwingend; der Container darf nicht direkt öffentlich erreichbar sein. Der Proxy muss Requestgrößen und Timeouts mindestens ebenso streng begrenzen, darf aber keine Autorisierungsidentität setzen.

## Explizit nicht enthalten

- öffentliche Registrierung, E-Mail-Verifikation, Recovery oder Kontolöschung,
- MFA, Cookies, Browser-Session- oder CSRF-Flows,
- öffentliche Sync- und Blob-Endpunkte,
- verteilte Rate Limits für mehrere Serverprozesse,
- Vertrauen in Proxy-IP-Header,
- Client-Keychain-/Credential-Manager-/Secret-Service-Integration.

## Folgen

Nachfolgende Sync- und Blob-Handler können denselben Bearer-abgeleiteten Principal verwenden. Bei horizontaler Skalierung müssen Abuse-Zähler und Refresh-Serialisierung neu bewertet werden; V1 bleibt gemäß Betriebsvertrag ein einzelner Serverprozess.
