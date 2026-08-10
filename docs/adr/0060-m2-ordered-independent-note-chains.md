# ADR 0060: Geordnete unabhängige Root-Note-Ketten

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Der Scheduler aus ADR 0057 wählte einmalig Kandidaten. Bei zwei aufeinanderfolgenden Remote-Revisionen derselben unabhängigen Root-Note konnte deshalb nur die erste Revision pro `SyncOnce` laufen, obwohl ihr erfolgreicher Abschluss die exakte Vorgänger-Baseline der zweiten Revision herstellte.

## Entscheidung

Der begrenzte Scheduler lädt nach jedem Fortschritt erneut höchstens 1000 Kandidaten. So darf eine spätere Root-Note-Update/-Delete-Revision erst in einer folgenden Auswahlrunde desselben Zyklus erscheinen. Alle bisherigen Guards bleiben unverändert: exakte Vorgänger-Baseline, keine frühere offene Zeile desselben Objekts, kein ungelöster lokaler Objekt-Intent, unveränderlicher Einzelplan und insgesamt höchstens 32 Applies pro Zyklus. Eine Runde ohne Fortschritt beendet den Drain und verhindert Schleifen durch lokal blockierte Kandidaten.

## Verifikation

Der Scheduler-Test nimmt eine Update-Kette Y@2→Y@3 hinter einer ungelösten X-Zeile auf, prüft beide Inbox-Zeilen als `applied`, die exakten finalen Y@3-Bytes, den weiterhin blockierten Confirmed-Frontier sowie nach Neustart unveränderten Inode und Änderungszeitpunkt. `TestIndependentInboxChainReselectsAfterReopen` unterbricht zusätzlich exakt nach dem dauerhaften Abschluss von Y@2, öffnet den Index neu und bindet erst dann Y@3 bei weiterhin blockiertem Präfix.

## Folgen

Unabhängige lineare Root-Note-Revisionsketten benötigen nicht mehr künstlich mehrere Vordergrundzyklen. Der Scope bleibt auf Root-Note-Updates/-Deletes beschränkt; die globale Apply-Grenze verhindert ungebundene Arbeit.
