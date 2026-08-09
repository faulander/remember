# ADR 0043: Recovery nichtleerer Folder-Creates mit direkten Notes

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Die bisherige Folder-Create-Recovery rettete ausschließlich leere Folder bei `path_collision` oder `parent_unavailable`. Ein nichtleerer Folder benötigt ein Subtree-Rekeying, weil seine abgeleiteten Create-Operationen noch den verworfenen Parent referenzieren.

## Entscheidung

Client-Schema v19 ergänzt ein unveränderliches Member-Manifest für den kleinsten sicher beweisbaren nichtleeren Fall. Automatische Recovery ist nur zulässig, wenn der vollständige indexierte Unterbaum aus mindestens einer direkten Note besteht und:

- jede Note genau einen nie versuchten pending Create besitzt,
- dieser Create ausschließlich direkt vom konfliktbehafteten Root-Create abhängt,
- UUID, Name, Blobhash, gestagte Bytes und lokale Frontmatter exakt übereinstimmen,
- keine Nested Folder, weiteren Nachfahren, Folgeoperationen oder externen aktiven Abhängigkeiten existieren.

Vor der Dateisystemmutation werden neue Root-UUID und neue Note-Operations-IDs dauerhaft gespeichert. Note-UUIDs und Bytes bleiben unverändert. Der exakte Root-Inode wird crash-resumierbar nach `_Konflikte/Wiederhergestellt` verschoben. Ein Scan-Manifest verifiziert vor und nach dem Move, dass der Subtree ausschließlich aus Root und erwarteten direkten Notes besteht.

Trusted Reconcile entfernt die alte Root-ID, bindet denselben Inode an die neue Root-ID, erhält Note-IDs und unterdrückt Capture ausschließlich für manifestierte Note-ID/Hash/Zielpfad-Paare. Während `moved` bleiben alte Note-Creates als Blob-Referenzen erhalten, blockieren aber weder Pull noch Recovery. Nach dauerhafter `ConflictRecoveredID`-Baseline werden atomar:

1. alte Note-Creates exakt gegen Manifest und pending Status superseded,
2. neuer Root-Create erzeugt,
3. neue Note-Creates unter dem neuen Root mit vorab gespeicherten Operations-IDs erzeugt,
4. Journal und bestehende Folder-Create-Auflösung abgeschlossen.

SQL-Guards verhindern unvollständige Manifeste und eine Auflösung mit verbleibenden aktiven direkten Abhängigkeiten. Windows bleibt wegen der bestehenden handle-sicheren Rooted-Move-Grenze fail-closed.

## Folgen

Ein häufiger nichtleerer Offline-Fall konvergiert ohne neue Note-UUID oder Byteverlust. Nested Folder, spätere Note-Edits und allgemeinere Subtrees bleiben unverändert fail-closed und benötigen das vollständige rekursive Rekey-Protokoll.
