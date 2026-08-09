# ADR 0046: Äquivalente konkurrierende Root-Note-Moves

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können dieselbe Root-Note offline auf exakt denselben Root-Namen verschieben. Der erste Move wird kanonisch, der zweite wird wegen seiner alten Basisrevision mit `base_revision_mismatch` abgelehnt. Eine Konfliktkopie wäre bei identischem Ziel unnötig; abhängige lokale Edits dürfen aber nicht verloren gehen.

## Entscheidung

Client-Schema v21 ergänzt die unveränderliche Auflösung `note_move_equivalent`. Sie gilt ausschließlich für einen Note-Move mit Root-Quelle, kanonischer nicht gelöschter Note, gültigem 32-Byte-Blob-Hash, strikt höherer Revision und exakt identischem Root-Zielnamen.

Vor der Auflösung prüft der Client, dass der lokale Index dieselbe Note-ID exakt am Ziel führt und dass die descriptor-sicher gelesene Markdown-Datei gültiges Frontmatter mit derselben Note-ID enthält. Es erfolgt keine Dateisystemmutation. Ein SQL-Trigger bindet die Auflösung an Operation, Typ, Konfliktcode, Root-Parent, Namen, Revision und Hashform.

`note_move_equivalent` erfüllt ausschließlich die gleichen Ready-, Attempt- und Result-Abhängigkeitsprüfungen wie der bereits abgesicherte `folder_move_reverted`-Fall. Dadurch bleibt ein nach dem lokalen Move erfasster Edit erhalten. Der exakte kanonische Move wird anschließend normal gepullt und wegen des bereits passenden lokalen ID-/Zielzustands ohne Dateisystemmutation angewendet.

Nicht-root-basierte Moves, divergente Ziele, Tombstones, Typabweichungen, nicht fortschreitende Revisionen und ungültige Hashzustände bleiben fail-closed.

## Verifikation

Tests prüfen zwei aktive Geräte, einen abhängigen lokalen Edit, die unveränderliche Auflösung, das Ausbleiben einer Konfliktkopie sowie Konvergenz auf Gerät A, Gerät B und einem kalten Gerät C. Ein Store-Test prüft Dependency-Freigabe und lehnt eine SQL-gespoofte divergente Auflösung ab.

## Folgen

Äquivalente konkurrierende Root-Note-Moves konvergieren ohne sichtbare Duplikate. Äquivalente nicht-root-basierte Moves bleiben getrennt, weil deren korrekter Pfad zusätzlich von authentifizierter Ancestor-Historie abhängt.
