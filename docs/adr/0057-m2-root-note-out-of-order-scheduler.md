# ADR 0057: Begrenzter out-of-order Scheduler für Root-Notes

## Status

Akzeptiert

## Kontext

ADR 0054–0056 trennen dauerhaft heruntergeladenen von zusammenhängend bestätigtem Pull-Fortschritt und binden einzelne Root-Note-Updates/-Deletes an persistente Apply-Pläne. Bislang beendete `SyncOnce` den Zyklus jedoch unmittelbar mit `ErrUnresolvedOutbound`; unabhängige Remote-Änderungen blieben deshalb praktisch blockiert.

## Entscheidung

Nur wenn nach dem vollständigen normalen Outbox-Submit weiterhin ein ungelöster lokaler Intent existiert, arbeitet `SyncOnce` in einem begrenzten Isolationsmodus:

1. Pull beginnt am dauerhaften `downloaded_cursor`, validiert weiterhin lückenlose Seiten und ingestiert höchstens 32 Seiten, ohne einen Legacy-Plan ab `confirmed_cursor` anzulegen.
2. Aus einem einmaligen, auf 1000 Zeilen begrenzten Kandidatensnapshot werden höchstens 32 unabhängige Änderungen angewendet.
3. Kandidaten sind ausschließlich pending Root-Note-`update`/`delete` mit exakter Baseline `revision-1`, ohne frühere nicht angewendete Inbox-Zeile desselben Objekts und ohne ungelösten lokalen Intent für dieses Objekt.
4. Jeder Kandidat erhält den unveränderlich gebundenen Einzelplan aus ADR 0056 und läuft durch den bestehenden Blob-, Preflight-, descriptor-sicheren Apply-, Reconcile- und Completion-Pfad.
5. Fehler lassen einen prepared/applying Plan dauerhaft zur Wiederaufnahme stehen. Der Scheduler ruft `AbandonPreparedInboxPlan` nicht automatisch auf.
6. `confirmed_cursor` rückt weiterhin nur über den zusammenhängenden applied-Präfix vor. Nach der begrenzten unabhängigen Arbeit bleibt `ErrUnresolvedOutbound` das sichtbare Ergebnis.
7. Sobald kein ungelöster Outbox-Zustand mehr existiert, bleibt der bisherige cursor-geordnete Legacy-Pfad maßgeblich. Replay-Seiten werden bis zum bereits dauerhaften `downloaded_cursor` begrenzt, damit keine Seite gespeicherten und neuen Verlauf mischt; bereits out-of-order angewendete Revisionen werden dort als applied geplant und nicht erneut im Dateisystem veröffentlicht.

Creates, Moves, Nested Notes und sämtliche Folder-Änderungen sind ausdrücklich ausgeschlossen. Spätere Revisionen desselben Objekts werden nicht in demselben Kandidatensnapshot nachgezogen.

## Folgen

Der erste enge Teil von `SYNC-012` ist aktiv: Ein ungelöster Konflikt kann unabhängige Root-Note-Updates und -Deletes nicht mehr global blockieren. `downloaded_cursor` darf vor `confirmed_cursor` liegen, ohne dass der bestätigte Präfix übersprungen wird. Vollständige Objektisolation, Strukturänderungen, Nutzersteuerung terminal abgebrochener Pläne und authentifizierte Mehrgeräte-Abnahme bleiben offen.
