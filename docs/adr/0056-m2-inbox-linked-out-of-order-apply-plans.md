# ADR 0056: Inbox-gebundene Einzelpläne für objektisolierten Apply

- Status: Angenommen
- Datum: 2026-08-10

## Kontext

Die Schema-v25-Inbox trennt Download- und Confirmed-Fortschritt, wird von `SyncOnce` bislang aber nur als Spiegel des strikt cursor-geordneten Legacy-Apply-Pfads verwendet. Für `SYNC-012` muss eine spätere, unabhängige Änderung angewendet werden können, ohne den bestätigten Cursor über eine frühere blockierte Änderung zu ziehen.

## Entscheidung

Schema v26 ergänzt `sync_inbox_apply_plans` als unveränderliche Eins-zu-eins-Bindung zwischen genau einer Inbox-Zeile und genau einem bestehenden Apply-Plan. SQL-Guards binden Cursor, Operation, Objekt, Mutation, Typ, Revision, Parent, Name und Blob exakt an einen einzigen Apply-Step und verhindern Link- oder Payload-Spoofing.

`Store.CreateInboxApplyPlan` ist absichtlich auf Root-Notizen mit `update` oder `delete` begrenzt. Die Inbox-Zeile muss pending sein, kein anderer Apply-Plan darf aktiv sein, es darf keine frühere nicht vollständig angewendete Änderung desselben Objekts geben und die persistierte Baseline muss exakt Revision minus eins sein. Folder, Creates, Moves und Nested Notes bleiben ausgeschlossen, weil ihre Pfad- beziehungsweise Ancestry-Abhängigkeiten noch kein sicheres Scheduling besitzen.

`BeginApplyPlan` setzt bei einem gebundenen Plan Plan und Inbox-Zeile atomar auf applying. `CompleteApplyPlan` verlangt genau einen angewendeten Step und erneut die exakte Baseline-Vorgängerrevision, aktualisiert die Objekt-Baseline, setzt Inbox und Plan atomar auf applied/completed und verschiebt `confirmed_cursor` ausschließlich über den zusammenhängenden applied-Präfix. Der Legacy-Planpfad bleibt unverändert.

Ein vorbereiteter gebundener Plan darf nur im vollständig unberührten Zustand ohne Folder-Publikations- oder Mutationsjournal dauerhaft auf failed gesetzt werden. Link und Inbox-Historie werden dabei nicht gelöscht.

## Folgen und Grenzen

Damit ist die persistente Ausführung einer späteren unabhängigen Root-Note-Änderung möglich, während eine frühere Inbox-Zeile den Confirmed-Frontier weiter blockiert. Ein Scheduler in `SyncOnce` wird in diesem Schnitt ausdrücklich nicht aktiviert. `SYNC-012` bleibt offen, bis Auswahl, Dateisystem-Apply, Konfliktisolation und Mehrgeräte-Abnahme integriert sind. Abgebrochene Planlinks bleiben zur forensischen Eindeutigkeit terminal gebunden und benötigen vor Scheduler-Aktivierung eine explizite Retry-/Operator-Regel.
