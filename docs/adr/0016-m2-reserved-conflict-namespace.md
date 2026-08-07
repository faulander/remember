# ADR 0016: Server-provisionierter reservierter Konfliktbereich

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Sichtbare Konfliktkopien sollen unter `_Konflikte/Wiederhergestellt` normale synchronisierte Notizen sein. Würden Clients diese Ordner unabhängig als gewöhnliche Objekte erzeugen, entstünden auf dem zweiten Gerät `object_exists`- oder Pfadkonflikte. Benutzeroperationen dürfen die reservierte Bedeutung außerdem nicht ersetzen, verschieben oder löschen.

## Entscheidung

Der Sync-Core definiert zwei protokollweit feste RFC-4122-Objekt-UUIDs für `_Konflikte` und dessen Kind `Wiederhergestellt`. Beim ersten persistierten Sync-Konflikt eines Kontos provisioniert der Server beide Folder atomar innerhalb derselben actor- und tenantgebundenen Transaktion. Die Provisionierung erzeugt normale Revision-1-Objektversionen und Change-Log-Einträge mit monotonen Cursors; Wiederholungen prüfen den exakten unveränderten Zustand und erzeugen keine Duplikate.

Öffentliche Mutationen mit einer der reservierten Folder-UUIDs werden unabhängig von der angeforderten Operation abgelehnt. Der bestehende reservierte Root-Name bleibt für gewöhnliche Objekte verboten. Normale Objekte dürfen dagegen unter der fest provisionierten `Wiederhergestellt`-UUID angelegt werden; damit werden spätere Konfliktkopien ohne Sonderformat synchronisiert.

Client und Server führen dieselben festen IDs und Namen als Protokollkonstanten. Die sichtbare lokale, crash-sichere Erstveröffentlichung dieser Ordner und die eigentliche Konfliktmaterialisierung folgen in einem separaten Schnitt; diese ADR allein lockert noch keine Outbox-Konflikte.

## Folgen

Der reservierte Bereich konvergiert konto- und geräteübergreifend über normale Pull-Änderungen, ohne konkurrierende Folder-Creates. Zwei zusätzliche Cursor entstehen nur beim ersten Konflikt eines Kontos. Eine beschädigte oder abweichende reservierte Serverstruktur lässt weitere Provisionierung geschlossen fehlschlagen und muss administrativ repariert werden.
