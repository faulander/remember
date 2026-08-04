# Remember Server

Minimaler modularer Go-Monolith für Remember. Der aktuelle Stand enthält Konfiguration, SQLite-Migrationen, Betriebsprobes, einen internen Identity-Core, einen actor-gebundenen Sync-Core und einen mandantengebundenen, auditierbaren SHA-256-Blob-Speicher. Es gibt weiterhin keine öffentlichen Konto-, E-Mail- oder Sync-Endpunkte.

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
| `REMEMBER_HTTP_READ_HEADER_TIMEOUT` | `5s` |
| `REMEMBER_HTTP_READ_TIMEOUT` | `15s` |
| `REMEMBER_HTTP_WRITE_TIMEOUT` | `30s` |
| `REMEMBER_HTTP_IDLE_TIMEOUT` | `60s` |
| `REMEMBER_SHUTDOWN_TIMEOUT` | `15s` |
| `REMEMBER_DB_BUSY_TIMEOUT` | `5s` |

Der Datenbankwert ist ein Dateipfad, kein frei konfigurierbarer SQLite-DSN. Datenbankdatei, Blob-Root und Staging-Root müssen verschieden sein; Blob und Staging müssen auf demselben lokalen Dateisystem liegen. Ein Blob ist in V1 fest auf **8 MiB** begrenzt. Vor Readiness werden abgebrochene Uploads bereinigt und alle registrierten Blobs vollständig geprüft.

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
- TLS wird an einem vorgeschalteten Reverse Proxy terminiert. Der Container darf nicht direkt öffentlich exponiert werden.
- Serverdatenträger und Backups müssen durch die Deployment-Umgebung verschlüsselt werden.
- Liveness hängt absichtlich nicht von SQLite ab; Readiness wird bei Datenbankfehlern oder während des Draining deaktiviert.
- Logs enthalten stabile Ereigniscodes, aber keine Datenbankpfade, Request-Inhalte, Querywerte, Dateinamen oder Notizinhalte.

## Tests

```bash
go test ./...
go test -race ./...
go vet ./...
```
