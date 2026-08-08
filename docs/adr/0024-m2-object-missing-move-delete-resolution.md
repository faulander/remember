# ADR 0024: `object_missing` für Note-Move und bereits erfüllte Deletes

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

ADR 0023 rettet lokale Note-Updates, deren UUID auf dem Server fehlt. Derselbe Zustand kann bei einem lokalen Move mit abhängigen Edits oder bei einem Delete auftreten. Ein Move enthält weiterhin Benutzerinhalt und Strukturabsicht; ein Delete ist dagegen bereits erfüllt, wenn das Remote-Objekt nicht existiert.

## Entscheidung

Ein Note-Move mit `object_missing` und `canonical: null` verwendet denselben materialisierten Rettungsweg wie ein Update: Die exakten Bytes am versuchten Ziel, einschließlich abhängiger lokaler Edits, werden unter neuer UUID gestaged, crash-sicher evakuiert und nach bestätigtem Wiederherstellungsordner als normale Konfliktnotiz synchronisiert. Weder der alte noch der versuchte Pfad wird mit der verwaisten UUID neu erzeugt.

Ein Delete mit `object_missing` und `canonical: null` wird dagegen als semantisch bereits erfüllt aufgelöst. Schema v9 ergänzt `sync_conflict_resolutions` mit der einzigen derzeit erlaubten Auflösung `already_deleted`. Der Eintrag referenziert ausschließlich eine unverändert gespeicherte konfliktbehaftete Delete-Operation, ist idempotent sowie durch Trigger update- und delete-geschützt. Es wird kein künstlicher Tombstone und keine inhaltslose Konfliktkopie erzeugt.

Abhängige pending/attempted Operationen werden rekursiv superseded. Unresolved- und Konfliktlisten ignorieren nur exakt journalisierte Auflösungen; der ursprüngliche Outbox-Eintrag behält `status='conflict'` und `conflict_code='object_missing'` als vollständige Historie. Da der Server bei jedem persistierten Konflikt den reservierten Bereich provisioniert, stellt der Client dessen lokale Identitäten auch für die No-op-Auflösung sicher bereit.

## Folgen

Fehlende Remote-Objekte führen für Move und Update zur sichtbaren Inhaltsrettung, für Delete dagegen zur dauerhaften semantischen Erfüllung. Die Unterscheidung vermeidet sowohl Datenverlust als auch unnötige Serverobjekte und Tombstones. Folder-Move- und Folder-Strukturkonflikte bleiben separat.
