# ADR 0078: Rekursive Note-Isolation in der Sync-Inbox

- Status: Angenommen
- Datum: 2026-08-13

## Entscheidung

Client-Schema v39 erweitert den begrenzten Out-of-order-Scheduler aus ADR 0057 und Schema v36 auf Note-Updates und -Deletes in beliebiger Folder-Tiefe bis einschließlich 256. Eine Inbox-Zeile ist nur kandidatfähig, wenn ihre vollständige aktuelle Folder-Ancestry lückenlos bis zum Root beweisbar ist.

Der Apply-Plan persistiert für jede Ancestor-Stufe Tiefe, Folder-ID, Parent-ID, relativen Pfad, Device/Inode sowie exakte Baseline-Revision und -Operations-ID. IDs dürfen in der Kette nicht wiederkehren. Jeder Ancestor muss bekannt, nicht ersetzt, synchronisiert und ohne wirksamen lokalen Intent sein; frühere offene Inbox-Zeilen oder Remote-Moves eines Ancestors schließen den Kandidaten aus. Die Note selbst benötigt weiterhin die exakte Vorgängerbaseline, keinen lokalen Intent und keine frühere offene Same-Object-Zeile.

Kandidatensicht und Plananlage prüfen dieselben Bedingungen transaktional. Vor jeder Dateisystemmutation, beim Resume eines `applying`-Plans und vor dem Abschluss wird die vollständige Ancestor-Kette erneut gegen den Index geprüft; der unmittelbare Parent wird zusätzlich über seine descriptor-relative Folder-Identität gebunden. Eine zwischen Planung und Apply verschobene, ausgetauschte oder in ihrer Baseline geänderte Ancestor-Stufe verwirft den Plan fail-closed; sie wird nicht über einen veralteten Pfad angewendet. Downloaded- und Confirmed-Cursor, lückenloser späterer Legacy-Abschluss und die Höchstgrenze von 32 Apply-Schritten pro Lauf bleiben unverändert.

Creates, Moves, Folder-Mutationen, Zyklen, fehlende oder unbekannte Ancestors, Tiefe über 256 und nicht vollständig gebundene Objektformen bleiben außerhalb des Schedulers pending. Der Scheduler löst den blockierenden Konflikt nicht auf; er isoliert ausschließlich beweisbar unabhängige Note-Änderungen dahinter.

## Folgen

Tief verschachtelte Note-Update/-Delete-Ketten können trotz eines unbeteiligten ungelösten Outbox-Konflikts über Pagination und Restarts fortschreiten, ohne den bestätigten Präfix vorzeitig zu verschieben. Store-, Trigger-, App- und Integrationsprüfungen decken Tiefe drei, fehlende oder ersetzte Ancestors, Zyklen, Baseline-/Intent-/Move-Abweichungen, Depth-Limit, Mutation zwischen Planung und Dateisystemschritt sowie A/B/Restart/Cold-C-Konvergenz mit exakten UUIDs und Bytes ab.
