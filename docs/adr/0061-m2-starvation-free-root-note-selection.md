# ADR 0061: Starvation-freie Root-Note-Auswahl

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Der ADR-0057-Scheduler begrenzt einen Kandidatenscan auf 1000 Zeilen. Die Auswahl filterte ungelöste lokale Intents erst danach in Go. Damit konnten 1000 lokal blockierte, ansonsten passende Zeilen eine unabhängige Zeile dahinter dauerhaft verdecken.

## Entscheidung

Schema v28 definiert `sync_unresolved_local_intents` als zentrale SQL-Sicht mit exakt den bisherigen Regeln für wirksame pending/attempted/replay-mismatch- und unaufgelöste Conflict-Outbox-Zustände. Kandidatenauswahl schließt diese Objekte vor `ORDER BY ... LIMIT` aus. `HasUnresolvedLocalIntent`, atomare Einzelplanerzeugung, der SQL-Link-Guard und der explizite Retry verwenden dieselbe Sicht. Die zusätzliche Go-Prüfung bleibt Defense in Depth.

## Verifikation

Ein Store-Test legt 1000 kandidatengleiche Zeilen mit ungelöstem lokalem Intent vor eine unabhängige Zeile. Nur die spätere Zeile wird ausgewählt und kann gebunden werden; direkte Planerzeugung für die erste blockierte Zeile wird abgelehnt. Bestehende SQL-Immutabilitäts-, Retry-, Migrations- und Scheduler-Tests bleiben maßgeblich.

## Folgen

Die bereits zugesagte Root-Note-Isolation kann nicht mehr an der Scan-Grenze verhungern. Der Slice erweitert keine unterstützte Objektform und ändert keine Konfliktpolitik.
