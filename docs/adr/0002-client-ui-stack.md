# ADR 0002: Wails mit Svelte und Vite

- **Status:** Akzeptiert
- **Datum:** 2026-08-02
- **Bezug:** `docs/DESIGN.md` Abschnitt 5.1, `M1-AC-003`

## Kontext

Die Desktop-UI wird vollständig statisch in Wails eingebettet. Für Meilenstein 1 werden weder serverseitiges Rendering noch dateibasiertes Routing, Load-Funktionen oder ein Node-Server benötigt.

## Entscheidung

Der Client verwendet:

- Wails 2,
- Svelte 5,
- TypeScript,
- Vite,
- statische Frontend-Artefakte unter `client/frontend/dist`.

SvelteKit wird vorerst nicht eingesetzt. Alle Dateisystem- und Prozesszugriffe bleiben in Go und werden ausschließlich über typisierte Wails-Bindings beziehungsweise Wails-Ereignisse aufgerufen.

## Konsequenzen

- Der Build bleibt kleiner und benötigt keine SvelteKit-Adapterkonfiguration.
- Routing kann später mit einer kleinen Client-Lösung ergänzt werden, falls mehrere Ansichten dies rechtfertigen.
- Ein späterer Wechsel zu SvelteKit bleibt möglich, ist aber erst bei konkretem Bedarf zu prüfen.
