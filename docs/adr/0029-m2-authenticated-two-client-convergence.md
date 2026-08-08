# ADR 0029: Authentifizierte Zwei-Client-Konvergenz im Integrationstest

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Client- und Server-Cores sowie ihre HTTP-Transporte waren separat getestet. Diese Tests belegten jedoch nicht, dass zwei dauerhafte lokale Roots über echte Access-Tokens, Blob-PUT/-GET, Operation-Submit und Cursor-Pull einschließlich serverseitig provisioniertem Konfliktbereich konvergieren.

Ein erster vollständiger Lauf zeigte zudem, dass ein dritter Client die vom Server provisionierten Ordner `_Konflikte/Wiederhergestellt` nicht anwenden konnte: Die allgemeine Benutzerpfadprüfung des Folder-Publikationsjournals lehnte den reservierten Pfad auch für seine festen Protokoll-IDs ab.

## Entscheidung

`server/integrationtest` komponiert ausschließlich für modulübergreifende Tests dieselben produktionsnahen Komponenten wie der Serverprozess: migriertes SQLite, Production-Identity und -Sessions, quota-gebundenes Blob-Repository, Sync-Core und den begrenzten HTTP-Handler. Benutzer werden intern registriert und verifiziert; Geräte melden sich anschließend ausschließlich über `POST /v1/auth/login` an.

`client/integration` startet zwei unabhängige `LocalCore`-Roots mit getrennten Access-Tokens und dem normalen `remotehttp.Client`. Der Test synchronisiert eine Notiz, erzeugt anschließend einen stale Update/Update-Konflikt und verlangt auf beiden Clients:

- dieselbe kanonische Serverfassung,
- denselben deterministischen Konfliktdateinamen,
- byteidentische sichtbare Konfliktkopien mit der lokalen Verliererfassung,
- erfolgreiche Übertragung über echte Blob- und Sync-Routen.

Das Folder-Publikationsjournal akzeptiert reservierte Pfade nur für zwei exakte Paare: `ConflictRootID` mit `_Konflikte` und `ConflictRecoveredID` mit `_Konflikte/Wiederhergestellt`. Alle anderen IDs, Pfade, Schreibweisen und reservierten Nachbildungen bleiben durch die normale Benutzerpfadprüfung abgelehnt.

## Folgen

Die zentrale Auth-/Blob-/Sync-Kette und ein repräsentativer Konflikt besitzen nun einen automatisierten Zwei-Geräte-Nachweis. Der Test ersetzt keine realen Plattformtests und schließt die verbleibende Folder-/Strukturkonfliktmatrix nicht ab. Das öffentliche Produkt erhält durch das Test-Harness keine neue Route oder Registrierungsoberfläche.
