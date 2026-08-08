# ADR 0033: Identitätsgebundener Revert von Folder-Move-Konflikten

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Gerät kann einen Folder lokal verschieben, während ein anderes Gerät den Zielpfad belegt oder den Ziel-Parent bereits gelöscht hat. Der Server lehnt den Move mit `path_collision` beziehungsweise `parent_unavailable` ab. Die lokale Folder-UUID und ihr gesamter Unterbaum bleiben fachlich gültig; ein Kopieren oder Rekeying aller Nachfahren wäre unnötig und würde Struktur verlieren.

Abhängige Änderungen an Kindern besitzen eigene Objekt-Basisrevisionen. Sie dürfen nach dem verworfenen Parent-Move weiterhin gesendet werden, sofern der Folder sicher an seinen authentifizierten kanonischen Pfad zurückkehrt. Spätere lokale Operationen auf demselben Folder würden dagegen auf einer nicht existierenden Move-Revision basieren und sind nicht automatisch rebasierbar.

## Entscheidung

Client-Schema v13 ergänzt das unveränderliche Journal `conflict_folder_move_reverts` und die Konfliktauflösung `folder_move_reverted`.

Ein Revert ist nur zulässig, wenn:

- die konfliktbehaftete Operation ein Folder-Move mit `path_collision` oder `parent_unavailable` ist,
- der kanonische Snapshot einen nicht gelöschten Folder mit exakt der Move-Basisrevision beschreibt,
- ein atomar mit der Outbox erfasster Quellpfad-/Device-/Inode-Intent existiert,
- keine spätere pending, attempted, replay-mismatch oder konfliktbehaftete Operation desselben Folders existiert,
- der aktuell verschobene Folder exakt dieselbe bekannte Device-/Inode-Identität trägt.

Der Client persistiert Operations-ID, Folder-ID, versuchten und kanonischen Pfad sowie Device/Inode vor der Dateisystemmutation. Der exakte Inode wird descriptor-relativ zum kanonischen Pfad zurückverschoben. Reconcile übersetzt nur die deterministischen Nachfahrenpfade dieses verifizierten Ancestor-Moves; abweichende externe Pfade oder Hashes werden nicht unterdrückt.

Das Journal ist monoton `prepared → moved → completed`, unveränderlich und nicht löschbar. Ein SQL-Insert-Guard erlaubt `folder_move_reverted` ausschließlich für ein abgeschlossenes, passendes Journal. Abstürze nach physischem Move oder nach Reconcile werden anhand von Inode und indexiertem Pfad unterschieden und idempotent wieder aufgenommen.

Nur diese konkrete Konfliktauflösung erfüllt Abhängigkeiten in Ready-, Attempt- und Result-Prüfungen. Dadurch können abhängige Kind-Creates/-Edits mit ihren unveränderten Objekt-Basisrevisionen fortfahren. Der konfliktbehaftete Folder-Move selbst bleibt als unveränderliche Historie erhalten.

## Folgen

Der Remote-Pfadgewinner beziehungsweise der Remote-Parent-Tombstone bleibt wirksam, während lokale Folder-Struktur und abhängige Kindinhalte erhalten bleiben. Mehrere aufeinanderfolgende lokale Operationen desselben konfliktbehafteten Folders bleiben bewusst fail-closed und benötigen einen separaten Rebase-Entwurf. Folder-Create-Kollisionen und `type_mismatch` bleiben ebenfalls separat.
