# ADR 0036: Wiederherstellung leerer Folder-Create-Pfadkollisionen

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können offline unterschiedliche neue Folder am selben portablen Pfad erzeugen. Der erste Create gewinnt serverseitig; der zweite erhält `path_collision` mit `canonical: null`, weil seine Folder-ID auf dem Server nicht existiert. Auch ein leerer Folder ist sichtbare Benutzerstruktur und darf nicht still verschwinden.

Ein nichtleerer lokaler Folder benötigt dagegen ein transaktionales Rekeying seines gesamten Unterbaums und aller abhängigen Outbox-Operationen. Dieser Schnitt beschränkt sich deshalb bewusst auf nachweislich leere Folder ohne spätere Operationen oder abhängige Objekte.

## Entscheidung

Client-Schema v16 ergänzt `conflict_folder_create_recoveries` und die Auflösung `folder_create_collision_recovered`.

Vor der Mutation verlangt der Client:

- Folder-Create, `path_collision` und keinen kanonischen Zustand,
- bekannte Device-/Inode-Identität,
- keine indexierten Nachfahren,
- descriptor-verifizierte Leere,
- keine späteren aktiven Operationen derselben Folder-ID und keine aktiven Abhängigkeiten.

Eine neue UUIDv7 und ein deterministischer Name mit vollständiger Konflikt-Operations-ID werden vor dem Move persistiert. Der exakte Folder-Inode wird exklusiv als direkter Kindordner nach `_Konflikte/Wiederhergestellt` verschoben. Die spezielle Empty-Move-Operation staged den Inode zuerst unter einem verborgenen Namen, prüft dort die Leere und stellt einen inzwischen nichtleeren Folder an der Quelle wieder her. Nach Veröffentlichung wird erneut geprüft; konkurrierend hinzugefügte Inhalte führen ebenfalls zur identitätsgebundenen Wiederherstellung statt zu einer falschen Empty-Recovery.

Trusted Reconcile ersetzt ausschließlich die ursprüngliche, nie serverseitig angelegte Folder-ID durch die persistierte Recovery-ID. Der konfliktbehaftete Outbox-Eintrag gilt im Zustand `moved` für den Pull nicht mehr als Blocker. So können der Server-Pfadgewinner und die reservierten Folder-Baselines angewendet werden. Sobald `ConflictRecoveredID` dauerhaft bestätigt ist, werden Recovery-Abschluss, neuer Folder-Create unter diesem Parent und unveränderliche Konfliktauflösung atomar gespeichert.

SQL-Insert-Guards verhindern vorgezogene oder fremd gebundene Journal-/Resolution-Zeilen. Beim Abschluss werden alle persistierten Identitätsfelder innerhalb derselben Transaktion erneut verglichen. Remote-Apply akzeptiert reservierte direkte Kindpfade nur, wenn der authentifizierte Apply-Schritt exakt `ConflictRecoveredID` als Parent trägt.

## Folgen

Leere verlierende Folder bleiben als sichtbare, synchronisierte Struktur erhalten. Der Server-Gewinner übernimmt den ursprünglichen Pfad. Nichtleere Folder-Create-Kollisionen bleiben fail-closed und unverändert am lokalen Pfad, bis ein vollständiges Subtree-Rekeying implementiert ist. Windows bleibt mangels handle-sicherer Empty-Move-Operation fail-closed.
