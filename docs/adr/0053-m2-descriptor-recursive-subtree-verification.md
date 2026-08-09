# ADR 0053: Descriptor-rekursive exakte Subtree-Verifikation

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Die bisherigen sicheren Folder-Recoveries verifizieren leere Folder oder streng manifestierte direkte Dateien. Eine spätere Recovery verschachtelter Folder-DAGs darf keinen Pfad rekursiv erneut öffnen und dabei Symlinks, Parent-Ersetzungen, unerwartete Einträge oder ausgetauschte Folder-Inodes übersehen.

## Entscheidung

Das Repository stellt auf Darwin/Linux `VerifyRootedSubtreeExpected` bereit. Ein Manifest beschreibt jeden Nachfahren relativ zu einem separat Device-/Inode-gebundenen Root-Folder als Folder mit erwarteter Device-/Inode-Identität oder als reguläre Datei mit erwartetem SHA-256.

Der Verifier:

- validiert portable relative Komponenten, Eindeutigkeit und lückenlos manifestierte Parent-Folder,
- öffnet den Root mit `O_NOFOLLOW`, bindet ihn per `fstat` an Device/Inode und hält den Descriptor,
- enumeriert rekursiv über gehaltene Deskriptoren und `openat(..., O_NOFOLLOW)`,
- fordert in jedem Folder exakte Anzahl, Namen und Typen,
- bindet jeden Nested Folder an Device/Inode,
- liest ausschließlich reguläre Dateien begrenzt und vergleicht den exakten SHA-256,
- mutiert das Dateisystem niemals.

Symlinks, Geräte, Sockets, fehlende oder zusätzliche Einträge, Hash-/Inode-Abweichungen und zu große Dateien werden abgelehnt. Eine Root-Pfadersetzung nach dem Öffnen beeinflusst die Traversierung nicht, weil der gehaltene Descriptor maßgeblich bleibt. Windows bleibt bis zu einer handle-sicheren rekursiven Implementierung explizit fail-closed.

## Verifikation

Deterministische Repository-Tests prüfen einen drei Ebenen tiefen Baum, unerwartete und fehlende Einträge, Typ-, Hash- und Inode-Abweichungen, Symlinks auf mehreren Ebenen, Größenbegrenzung, ungültige Manifestpfade und eine Root-Ersetzung nach dem Descriptor-Open.

## Folgen

Der Sicherheitsbaustein für rekursive Nested-Folder-Recovery ist vorhanden. Diese ADR implementiert bewusst noch kein Subtree-Rekeying, kein Recovery-Journal und keine neue Konfliktzelle.
