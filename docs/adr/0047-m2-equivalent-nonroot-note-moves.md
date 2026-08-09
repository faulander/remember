# ADR 0047: Äquivalente konkurrierende Nicht-Root-Note-Moves

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können dieselbe Note offline aus derselben Basisrevision auf exakt denselben Namen im exakt selben Parent-Folder verschieben. ADR 0046 löste diesen Fall zunächst nur am Root. Für verschachtelte Ziele darf der Client den Parent-Pfad nicht aus unvollständiger Historie erraten.

## Entscheidung

Client-Schema v22 erweitert `note_move_equivalent` auf exakte nullable Parent-Gleichheit zwischen unveränderlichem Outbox-Move und authentifiziertem kanonischem Konfliktzustand. Für Nicht-Root-Ziele gilt zusätzlich:

- die Parent-ID ist lokal als Folder mit bekannter Device-/Inode-Identität indexiert,
- derselbe Inode wird descriptor-sicher unter dem indexierten Parent-Pfad verifiziert,
- für den Parent existiert keine ungelöste lokale Absicht,
- der aus dieser verifizierten ID-Struktur abgeleitete Zielpfad entspricht exakt dem indexierten Note-Pfad,
- die dort descriptor-sicher gelesene Markdown-Datei trägt dieselbe Note-ID.

Es erfolgt keine Dateisystemmutation. Die SQL-Auflösung verlangt weiterhin aktive kanonische Note, `base_revision_mismatch`, strikt höhere Revision, gültigen Hash, identischen Namen und nun exakt gleiche nullable Parent-ID. Abhängige Note-Edits werden wie bei ADR 0046 freigegeben.

Ein fehlender oder identitätsunklarer Parent, ein Parent mit lokaler Absicht, abweichender Pfad, abweichender Parent oder Name, Tombstone, ungültiger Hash und nicht fortschreitende Revision bleiben fail-closed. Root-Verhalten aus ADR 0046 bleibt unverändert.

## Verifikation

Ein Mehrgeräte-Test verschiebt dieselbe verschachtelte Note auf A und B an dasselbe Ziel, erhält einen abhängigen Edit auf B und prüft A/B sowie kaltes C. Store-Tests prüfen Dependency-Freigabe und lehnen abweichende Parent-IDs SQL-gebunden ab.

## Folgen

Äquivalente Note-Moves konvergieren nun für Root- und sicher verifizierbare Nicht-Root-Ziele ohne Konfliktkopie. Divergente Ziele sowie Ancestor-Konflikte bleiben getrennt.
