# Remember Server

Minimaler modularer Go-Monolith für Remember. Der aktuelle Stand enthält Konfiguration, SQLite-Migrationen, Betriebsprobes, Identity-, Sessions- und Sync-Cores sowie einen mandantengebundenen, auditierbaren SHA-256-Blob-Speicher. Öffentlich verfügbar sind begrenzte Auth-, Blob- und Sync-Transporte; Registrierung, E-Mail-Verifikation und Recovery bleiben intern.

## Lokal starten

```bash
cd server
go run ./cmd/remember-server
```

Standardwerte:

- HTTP: `127.0.0.1:8080`
- SQLite: `data/sqlite/remember.db`
- Blob-Speicher: `data/blobs`
- Upload-Staging: `data/staging`
- Liveness: `GET /healthz`
- Readiness: `GET /readyz`

## Konfiguration

| Variable | Standard |
|---|---|
| `REMEMBER_LISTEN_ADDR` | `127.0.0.1:8080` |
| `REMEMBER_DB_PATH` | `data/sqlite/remember.db` |
| `REMEMBER_BLOB_ROOT` | `data/blobs` |
| `REMEMBER_STAGING_PATH` | `data/staging` |
| `REMEMBER_USER_BLOB_QUOTA_BYTES` | `1073741824` (1 GiB) |
| `REMEMBER_HTTP_READ_HEADER_TIMEOUT` | `5s` |
| `REMEMBER_HTTP_READ_TIMEOUT` | `15s` |
| `REMEMBER_HTTP_WRITE_TIMEOUT` | `30s` |
| `REMEMBER_HTTP_IDLE_TIMEOUT` | `60s` |
| `REMEMBER_SHUTDOWN_TIMEOUT` | `15s` |
| `REMEMBER_DB_BUSY_TIMEOUT` | `5s` |

Der Datenbankwert ist ein Dateipfad, kein frei konfigurierbarer SQLite-DSN. Datenbankdatei, Blob-Root und Staging-Root müssen verschieden sein; Blob und Staging müssen auf demselben lokalen Dateisystem liegen. Ein Blob ist in V1 fest auf **8 MiB** begrenzt. Die logische Benutzerquota zählt alle eindeutigen berechtigten Inhaltsversionen, beträgt standardmäßig 1 GiB und darf höchstens 1 TiB betragen. Vor Readiness werden abgebrochene Uploads bereinigt und alle registrierten Blobs vollständig geprüft.

## Öffentliche HTTP-Routen

Neben `/healthz` und `/readyz` stellt der Server derzeit den begrenzten M2-Auth-, Blob- und Sync-Transport bereit:

- `POST /v1/auth/login`
- `POST /v1/auth/refresh`
- `POST /v1/auth/logout`
- `GET /v1/sessions`
- `PATCH|DELETE /v1/devices/{uuidv7}`
- `DELETE /v1/sessions/{uuidv7}`
- `PUT|GET /v1/blobs/{sha256}`
- `POST /v1/sync/operations`
- `GET /v1/sync/changes?after={cursor}&limit={n}`

Sync-Operationen sind einzeln und idempotent; fachliche Konflikte bleiben erfolgreiche Antworten. Pull verwendet benutzerspezifische Cursor und höchstens 500 Änderungen pro Seite.

Blob-PUT verlangt einen unkomprimierten `application/octet-stream` mit bekannter Länge bis 8 MiB; Blob-GET ist mandantengebunden und nicht cachebar. Fremde, unbekannte und nicht berechtigte Hashes sind öffentlich nicht unterscheidbar. Range- und Conditional-Requests werden noch nicht unterstützt.

JSON-Requests sind auf 16 KiB begrenzt und werden strikt validiert. Login und Refresh besitzen feste globale und geheimnisgebundene Mindest-Rate-Limits; `429` liefert `Retry-After`. Der Limiter speichert nur SHA-256-Schlüssel und wertet weder `X-Forwarded-For` noch andere Client-IP-Header aus. Registrierung und Recovery sind noch nicht öffentlich; Batch-Sync, Streaming und Server Push sind nicht enthalten.

## Docker

```bash
docker build -t remember-server:test ./server
docker volume create remember-data
docker run --rm --name remember-server \
  -p 127.0.0.1:8080:8080 \
  -v remember-data:/data \
  remember-server:test
```

Der Container läuft als numerischer Nicht-Root-Benutzer `65532` und erwartet ein beschreibbares lokales Volume unter `/data`. Bei Bind-Mounts muss der Administrator die Besitzrechte passend setzen.

## Betriebsgrenzen

- Genau **eine** Serverinstanz darf auf die SQLite-Datei schreiben.
- SQLite und spätere Blobs benötigen ein zuverlässiges lokales Linux-Dateisystem; Netzwerkdateisysteme werden nicht unterstützt.
- TLS wird zwingend an einem vorgeschalteten Reverse Proxy terminiert. Der Container darf nicht direkt öffentlich exponiert werden; Proxy-Header verleihen keine Autorität.
- Serverdatenträger und Backups müssen durch die Deployment-Umgebung verschlüsselt werden.
- Liveness hängt absichtlich nicht von SQLite ab; Readiness wird bei Datenbankfehlern oder während des Draining deaktiviert.
- Logs enthalten stabile Ereigniscodes, aber keine Datenbankpfade, Request-Inhalte, Querywerte, Dateinamen oder Notizinhalte.

## Tests

```bash
go test ./...
go test -race ./...
go vet ./...
```
