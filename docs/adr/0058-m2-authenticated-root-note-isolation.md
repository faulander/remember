# ADR 0058: Authentifizierte Root-Note-Isolation hinter divergentem Folder-Move

## Status

Akzeptiert

## Kontext

ADR 0057 aktiviert den begrenzten out-of-order Scheduler zunächst für unabhängige Root-Note-Updates und -Deletes. Die bisherige Abnahme verwendete einen In-Memory-Transport. Zudem leitete der Konfliktdispatcher jeden Folder-Move-`base_revision_mismatch` an den Resolver für äquivalente Ziele weiter; bei einem bewusst divergenten Ziel gab dieser einen technischen Validierungsfehler zurück, bevor der isolierte Pull beginnen konnte.

## Entscheidung

Ein divergenter Folder-Move-`base_revision_mismatch` wird weiterhin nicht automatisch aufgelöst, aber vom Konfliktdispatcher als bewusst nicht unterstützter, dauerhaft ungelöster Outbox-Zustand behandelt. Nur ein exakt äquivalentes kanonisches Parent-/Name-Ziel wird an den bestehenden Resolver übergeben. Damit erreicht `SyncOnce` den Isolationsmodus aus ADR 0057 und liefert nach begrenztem Fortschritt weiterhin sichtbar `ErrUnresolvedOutbound`.

`TestAuthenticatedRootNoteIsolationBehindDivergentFolderMove` prüft über echte Login-, Blob- und Sync-HTTP-Routen:

- A und B teilen Folder X sowie Root-Notes Y und Z;
- A verschiebt X nach `RemoteFolder`, aktualisiert Y mit exakt geprüften Bytes und löscht Z;
- B verschiebt X offline nach `LocalFolder`; der divergente Move bleibt mit unverändertem Inode und ungelöstem Intent fail-closed;
- B lädt den Remote-Verlauf vollständig, hält `confirmed_cursor` vor X und markiert die unabhängigen Y-/Z-Zeilen dennoch `applied`;
- Neustart beginnt wieder am `downloaded_cursor` und publiziert Y nicht erneut;
- eine unabhängige lokale Root-Note Q von B wird trotz X übermittelt;
- A und ein kalter Client C sehen das kanonische `RemoteFolder`, Y, den Z-Tombstone und Q mit exakten IDs und Bytes.

## Folgen

Der enge ADR-0057-Scope besitzt nun eine produktionsnahe A/B-/Cold-C-Abnahme. Divergente Folder-Moves bleiben ausdrücklich fail-closed; geändert wurde nur ihre Einordnung als erwarteter ungelöster Konflikt statt als technischer Abbruch vor Pull. `SYNC-012` bleibt wegen Creates, Moves, Nested Notes, Folder-Apply und weiterer Objektformen teilweise offen.
