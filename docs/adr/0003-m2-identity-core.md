# ADR 0003: Identity-Core für Meilenstein 2

- **Status:** Akzeptiert für den internen Core
- **Datum:** 2026-08-03
- **Bezug:** `ACC-001`–`ACC-004`, `SEC-002`, `SEC-007`

## Scope

Diese Entscheidung gilt für den internen Identity-Core. Öffentliche HTTP-Endpunkte, E-Mail-Versand, Sessions, Recovery und Rate Limits folgen in getrennten Schnitten. Eine öffentliche Registrierung darf erst mit Zustelladapter, Enumeration-Schutz und Mindest-Rate-Limits aktiviert werden.

## E-Mail

- Eingaben müssen gültiges UTF-8 sein.
- Nur äußere ASCII-Leerzeichen und Tabs werden entfernt; Steuerzeichen werden abgelehnt.
- V1 akzeptiert ausschließlich einen ASCII-Dot-Atom-Local-Part ohne Anzeigenamen, Kommentare, quoted local part oder Domain-Literal.
- Local Part: maximal 64 Bytes, keine führenden, abschließenden oder doppelten Punkte.
- Domain: IDNA Lookup Profile, als lowercase A-Label gespeichert, maximal 253 Bytes.
- Gesamtadresse: maximal 254 Bytes.
- Kanonischer Schlüssel: lowercase Local Part plus `@` plus lowercase A-Label-Domain.
- Keine providerabhängige Punkt- oder `+tag`-Behandlung.
- SMTPUTF8-Local-Parts bleiben eine spätere Produktentscheidung.

## Konten und IDs

Zustände:

1. `pending_verification`
2. `active`
3. `deletion_pending`

Neue Benutzer-IDs sind UUIDv7 nach RFC 9562 und werden als 16-Byte-BLOB gespeichert. Nur aktive Konten dürfen später Sitzungen oder Sync verwenden.

## Passwort-Policy v1

- gültiges UTF-8, keine Normalisierung oder stille Kürzung,
- mindestens 15 Unicode-Codepoints,
- maximal 1024 UTF-8-Bytes,
- Argon2id v19,
- Memory: 65.536 KiB,
- Iterationen: 3,
- Parallelität: 1,
- Salt: 16 Bytes,
- Hash: 32 Bytes,
- PHC-String plus explizite Policy-Version `1`,
- konstante Hashvergleiche und begrenzter PHC-Parser.

Die Parameter werden vor öffentlicher Registrierung im realen Linux-Container benchmarked. Rate Limits und begrenzte gleichzeitige Argon2-Arbeit sind vor einem öffentlichen Endpoint Pflicht.

## E-Mail-Verifikation

- 32 zufällige Bytes aus `crypto/rand`,
- ungepaddetes Base64url mit exakt 43 Zeichen,
- Speicherung ausschließlich als SHA-256 über `remember:email-verification:v1\0 || rawToken`,
- genau 24 Stunden gültig,
- höchstens ein aktuelles Token pro Konto,
- Prüfung, Aktivierung und Tokenlöschung in einer Transaktion,
- bei `now == expires_at` abgelaufen,
- unbekannt, abgelaufen, wiederverwendet und falscher Zweck liefern denselben fachlichen Fehler.

Token, Tokenhash, E-Mail und Passwortdaten dürfen nicht geloggt werden.

## Doppelte Registrierung

Der kanonische E-Mail-Schlüssel ist eindeutig. Das Passwort wird vor dem Insert gehasht. Bei jedem bestehenden Kontozustand werden weder Passwort, Zustand, User-ID noch Token verändert oder offengelegt. Eine spätere öffentliche API antwortet unabhängig vom Ergebnis identisch. Token-Neuausstellung ist eine separate, rate-limitierte Operation.

## Zeit und Repository-Grenzen

- Serverzeit über eine injizierbare Clock, pro Operation genau einmal gelesen, UTC als Unix-Millisekunden.
- Identity-Lookups nach kanonischer E-Mail oder Token sind eng begrenzte globale Bootstrap-Ausnahmen.
- Künftige fachliche Repositories werden über `Store.ForUser(AuthenticatedUserID)` mandantengebunden; Methoden akzeptieren keine überschreibbare User-ID.
- Künftige Objekt-Primärschlüssel enthalten `(user_id, object_id)` und erhalten Cross-Tenant-Tests.
