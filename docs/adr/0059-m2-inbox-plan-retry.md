# ADR 0059: Expliziter Retry für abgebrochene Inbox-Pläne

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

ADR 0056 bindet eine Inbox-Zeile unveränderlich an genau einen Einzel-Apply-Plan. Ein sicher vor dem Apply abgebrochener Plan blieb im Zustand `failed` terminal gebunden und konnte deshalb nicht erneut ausgeführt werden.

## Entscheidung

Client-Schema v27 ergänzt den streng bewachten Übergang `failed → prepared`. `Store.RetryAbandonedInboxPlan` erlaubt ihn nur, wenn Inbox und einziger Step weiterhin `pending` sind, keine Apply-Folder-Journale existieren, kein anderer Plan aktiv ist, keine frühere Änderung desselben Objekts offen ist und die Objekt-Baseline weiterhin exakt der Vorgängerrevision entspricht. Planlink und Payload bleiben unveränderlich; es wird kein neuer Plan und keine neue Operations-ID erzeugt.

Der aktive Scheduler bricht Pläne weiterhin nicht automatisch ab. Retry bleibt eine explizite technische beziehungsweise spätere Operator-Aktion.

## Verifikation

Store- und SQL-Tests prüfen Abbruch, fehlenden aktiven Plan, unveränderte pending Inbox, erfolgreichen Retry desselben Planlinks sowie die bestehenden Zustands-, Baseline- und Spoofing-Guards.

## Folgen

Terminale Bindung verhindert weiterhin stilles Ersetzen, blockiert aber keinen dauerhaft bewahrten manuellen Retry mehr. Eine UI-/Operator-Steuerung bleibt offen.
