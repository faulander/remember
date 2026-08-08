# ADR 0019: Edit-vs-Delete-Konflikte mit Tombstone-Vorrang

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Client kann eine Notiz auf einer alten Basis bearbeiten, nachdem ein anderes Gerät sie bereits gelöscht hat. Die Löschung soll gemäß Produktentscheidung wirksam bleiben, während die konkurrierende lokale Fassung sichtbar und synchronisiert gerettet wird. Zwischen der lokalen Basis und dem finalen Tombstone können weitere kanonische Updates oder Moves liegen und über mehrere Pull-Seiten verteilt eintreffen.

## Entscheidung

Ein abgelehntes lokales Note-Update mit Konfliktcode `object_deleted` wird über das Materialisierungsjournal aus ADR 0017 gesichert und mit neuer Notiz-ID unter `_Konflikte/Wiederhergestellt` vorbereitet. Die Ausnahme für den Remote-Apply ist ausschließlich an den gespeicherten kanonischen Tombstone gebunden: Objekt-ID, Typ, Revision, Parent, Name und Blob-Hash müssen dem gepullten finalen Delete exakt entsprechen.

Kanonische Note-Updates und -Moves mit kleineren Revisionen desselben Objekts werden während einer aktiven Delete-Materialisierung im Apply-Journal als verarbeitet markiert, aber nicht auf die lokale Konfliktfassung veröffentlicht. Dies gilt innerhalb einer Pull-Seite und über Seitengrenzen hinweg. Der finale Delete verschiebt stattdessen die exakt hashgeprüften lokalen Bytes vom ursprünglich gebundenen Pfad in den recoverable Trash. Damit bleibt die lokale Fassung sowohl technisch im Trash als auch als neu identifizierte, später sichtbare Konfliktkopie erhalten.

Die Konfliktkopie wird erst veröffentlicht, wenn die Tombstone-Revision dauerhaft bestätigt ist, die ursprüngliche Objekt-ID aus dem Index verschwunden ist und der ursprüngliche Pfad tatsächlich nicht mehr existiert. Bereits als angewendet journalisierte Zwischenrevisionen dürfen nach einem Absturz fehlen; nicht angewendete Schritte bleiben streng vorprüfbar. Ein Neustart nach Dateisystem-Delete, nach Reconcile oder vor dem Apply-Marker nimmt denselben Plan wieder auf.

## Folgen

Remote-Löschungen gewinnen deterministisch gegen lokale Updates, ohne lokale Markdown-Inhalte zu verlieren. Zwischenliegende Remote-Moves müssen nicht kurzzeitig lokale Benutzerdaten überschreiben. Der umgekehrte Fall – lokaler Delete gegen Remote-Edit – benötigt weiterhin einen separaten Rebase-Schritt und ist nicht Bestandteil dieser Entscheidung.
