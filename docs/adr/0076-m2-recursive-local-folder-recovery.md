# ADR 0076: Rekursive lokale Folder-Recovery

- Status: Angenommen
- Datum: 2026-08-13

## Entscheidung

Client-Schema v38 erweitert die lokalen Recovery-Pfade für Folder-Create-Pfadkollision beziehungsweise gelöschten Parent, divergenten Root-Folder-Move und lokalen Folder-Move gegen Remote-Delete auf vollständig manifestierte Nested-Folder-DAGs. Zulässig sind ausschließlich pending, nie versuchte Folder- und Note-Creates sowie je Note höchstens eine lineare pending, nie versuchte Update-Kette. Jedes Kind muss genau von der Operation seines unmittelbaren Parents abhängen; Baselines, externe Objekthistorie, Branches, Moves und Deletes im erhaltenen Subtree schließen die Recovery aus.

Der lokale Verlierer-Root erhält unter `_Konflikte/Wiederhergestellt` eine neue Folder-UUID. Bereits erzeugte UUIDs aller manifestierten Nachfahren bleiben erhalten. Direkte Kinder werden auf den neuen Root umgebunden, tiefere Parent-IDs bleiben unverändert. Sämtliche ersetzten Root-, Folder-, Note- und Update-Operationen erhalten frische Operations-IDs; Notes werden auf genau einen finalen Create mit ihrer letzten manifestierten Blob-Fassung reduziert. Folder-Replacements werden parent-first geordnet.

Vor der Umschreibung erfasst der Client den vollständigen DAG mit relativen Pfaden, Typen, Parent-IDs, Tiefen, alten und neuen Operations-IDs, Folder-Device/Inode sowie initialen und finalen Note-Hashes in einer versiegelten Resolution. Descriptor-relative Subtree-Verifikation muss dieses Manifest exakt bestätigen. Danach ersetzt eine lokale Transaktion die alten Intents und Dependency-Kanten; der existierende crash-fortsetzbare Staging-, Reconcile- und Publication-Pfad materialisiert den neuen Root und führt die neuen Operationen aus. Ein Restart lädt ausschließlich das versiegelte Manifest und darf keine neuen Zuordnungen erzeugen.

Unerwartete oder fehlende Einträge, Symlinks, ausgetauschte Folder-Identitäten, Parent-/Pfadabweichungen, unmanifestierte Historie, attempted oder verzweigte Ketten sowie Descendant-Move/-Delete bleiben fail-closed. Der direkte leere beziehungsweise Direct-Note-Pfad bleibt für nicht rekursive Manifeste unverändert. Windows bleibt wegen fehlender handle-basierter Reparse-Point-Sicherheit fail-closed.

## Folgen

Die bisher in ADR 0043, 0050 und 0051 ausgeschlossenen Nested-Folder-Fälle konvergieren für den oben exakt begrenzten lokalen Create/Update-DAG. Rekursive oder nichtlineare divergente Historie und bereits serverbekannte Descendants sind weiterhin kein Bestandteil dieser Entscheidung.

Tests belegen stabile Manifest- und Replacement-IDs über Reopen, Ablehnung versuchter oder baseline-gebundener Descendants, parent-first Apply sowie Folder-Create-, divergente Folder-Move- und Move/Remote-Delete-Recovery. Die authentifizierte Create-Abnahme prüft A/B, Restarts und einen kalten Client mit erhaltenen Note-UUIDs und exakten finalen Bytes.
