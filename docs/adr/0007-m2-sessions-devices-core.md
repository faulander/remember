# ADR 0007: Interner M2-Sessions- und Devices-Core

- Status: Angenommen
- Datum: 2026-08-04

## Kontext

Identity, Sync und Blob-Speicher sind intern implementiert. Der nächste M2-Schnitt muss erfolgreiche Zugangsdaten in eine widerrufbare, strikt mandantengebundene Geräte- und Sitzungshierarchie überführen. Öffentliche Auth-Endpunkte dürfen weiterhin erst gemeinsam mit Enumeration-Schutz und Mindest-Rate-Limits entstehen.

## Entscheidung

Der interne Core liegt in `server/internal/session` und erhält Datenbank, Uhr, Zufallsquelle und einen Credential-Authenticator als injizierte Abhängigkeiten.

### Tokens und Laufzeiten

- Access- und Refresh-Tokens sind opake 32-Byte-Zufallswerte, kanonisch als Base64url ohne Padding kodiert.
- Access-Tokens sind 15 Minuten gültig.
- Eine Refresh-Familie entspricht genau einer Sitzung auf einem Gerät und hat eine absolute Laufzeit von 30 Tagen ab Login. Rotation verlängert diese Frist nicht.
- In SQLite werden ausschließlich domain-separierte SHA-256-Hashes gespeichert (`remember:access-token:v1` beziehungsweise `remember:refresh-token:v1`). Rohe Tokens werden nur einmal an den internen Aufrufer zurückgegeben.
- Zeitprüfungen lehnen sowohl abgelaufene als auch aus Sicht der Serveruhr erst zukünftig ausgestellte Tokens ab.

### Rotation und Replay

Refresh-Tokens sind einmal verwendbar. Eine erfolgreiche Rotation konsumiert den Vorgänger transaktional, verknüpft genau einen Nachfolger und erzeugt ein neues Access-Token. Die erneute Verwendung eines konsumierten Vorgängers gilt als Replay und widerruft die gesamte Sitzung einschließlich aller Access-Tokens. Der produktive Einzelprozess serialisiert Refresh-Übergänge zusätzlich; SQLite bleibt die dauerhafte Quelle der Zustände.

Fehler bei Zufallserzeugung oder Datenbanktransaktion dürfen keinen teilweise konsumierten Token hinterlassen.

### Geräte, Sitzungen und Mandantenbindung

Ein Login mit verifizierten Zugangsdaten erzeugt serverseitig je eine UUIDv7 für Gerät und Sitzung. Gerätenamen werden getrimmt und müssen gültiges UTF-8, nicht leer, höchstens 100 Unicode-Codepoints und höchstens 256 UTF-8-Bytes lang sein.

Ein Principal wird ausschließlich durch den Hash eines gültigen Access-Tokens aus der Datenbank abgeleitet und enthält Benutzer-, Geräte- und Sitzungs-ID. Listen, Umbenennen und Widerrufen leiten den Tenant aus diesem Principal ab; eine vom Aufrufer frei gesetzte Benutzer-ID ist keine Autorisierung. Composite Foreign Keys binden Tokens und Sitzungen konsistent an denselben Benutzer und dasselbe Gerät.

Sitzungen und Geräte können unmittelbar widerrufen werden. Gerätewiderruf widerruft alle zugehörigen Sitzungen und Access-Tokens. Kontolöschung entfernt Geräte, Sitzungen und Tokens kaskadierend. Ungültige, abgelaufene, widerrufene sowie konto- oder geräteseitig inaktive Credentials schlagen nach außen generisch fehl.

Credential-Prüfung führt für unbekannte, syntaktisch ungültige und inaktive Konten ebenfalls begrenzte Argon2-Arbeit aus und gibt nur `ErrInvalidCredentials` zurück.

## Explizit nicht enthalten

Dieser Schnitt enthält noch nicht:

- HTTP-Endpunkte, Cookies, Header-Parsing oder öffentliche Registrierung,
- Login-/Registrierungs-/Refresh-Rate-Limits und Transport-Enumeration-Schutz,
- Recovery, MFA, JWT oder Schlüsselrotation,
- Clientintegration oder Speicherung in Keychain, Credential Manager beziehungsweise Secret Service,
- öffentliche Sync- oder Blob-Endpunkte.

Vor einem öffentlichen Auth-Transport werden Sessions mit Mindest-Rate-Limits, Request-Größenlimits, sicheren Fehlerabbildungen und der plattformspezifischen sicheren Clientablage zusammengeführt.

## Folgen

Sync- und Blob-Transport können später einen intern abgeleiteten `Principal` verwenden, ohne Tenant-IDs aus Requestdaten zu vertrauen. Refresh-Replay beendet bewusst auch eine gerade erfolgreich rotierte Sitzung; der Benutzer muss sich anschließend neu anmelden. Tokenhistorie bleibt bis zur Sitzungslöschung für diese Erkennung erhalten.
