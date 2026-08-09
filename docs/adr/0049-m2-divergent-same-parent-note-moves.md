# ADR 0049: Divergente Nicht-Root-Note-Moves innerhalb derselben Parent-ID

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können dieselbe verschachtelte Note offline innerhalb desselben Folders auf unterschiedliche Namen verschieben. Der zuerst angenommene Move bestimmt den kanonischen Namen; der zweite wird mit `base_revision_mismatch` und einem authentifizierten kanonischen Note-Zustand abgelehnt. Die lokale Zielwahl und ein davon abhängiger Edit dürfen nicht verloren gehen.

## Entscheidung

Die bestehende crash-resumierbare Materialisierung divergenter Root-Note-Moves wird ausschließlich auf Nicht-Root-Moves erweitert, wenn Outbox und kanonischer Zustand dieselbe nichtleere Parent-ID, unterschiedliche Namen, den Note-Typ, einen aktiven kanonischen Zustand, einen 32-Byte-Blob-Hash und eine strikt höhere Revision belegen.

Vor jeder lokalen Konfliktordner-Publikation muss der Client zusätzlich nachweisen:

- Die Parent-ID bezeichnet im lokalen Index einen bekannten Folder mit nichtnull Device-/Inode-Identität.
- Für den Parent besteht keine ungelöste lokale Outbox-Absicht.
- Der Parent-Inode ist descriptor-relativ am indexierten Pfad verifizierbar.
- Der aus indexiertem Parentpfad und Outbox-Namen abgeleitete Zielpfad gehört exakt zur betroffenen Note-ID.
- Die dort gelesene Markdown-Datei enthält dieselbe Note-ID.

Die kanonische Gewinnerfassung wird bei Nicht-Root-Zielen ausschließlich über einen gehaltenen, per `fstat` an Device/Inode gebundenen Parent-Descriptor publiziert; ein zwischenzeitlich ersetzter Parent-Pfad bleibt ohne Mutation fail-closed. Danach verwendet der Client unverändert das bestehende monotone `conflict_materializations`-Journal: Die lokale Fassung wird hashgebunden staged und evakuiert, unter neuer UUID in `_Konflikte/Wiederhergestellt` publiziert und synchronisiert; die kanonische Note-ID erscheint nach Pull am Servernamen. Ein abhängiger lokaler Edit ist Bestandteil der geretteten Bytes.

Es ist keine neue Migration erforderlich. Das vorhandene Journal bindet Operations-ID, Quellobjekt-ID, ursprünglichen Pfad, Quellhash, neue Konfliktnotiz-ID, Zielpfad und materialisierten Hash bereits vor der Evakuierung unveränderlich. Die zusätzliche Parent-/Pfadprüfung ist eine Zulässigkeitsprüfung vor Erstellung dieses Journals; nach dessen Erstellung ist die zu rettende Fassung unabhängig vom Parent vollständig im hashadressierten Staging gebunden.

Unterschiedliche Parent-IDs, fehlende oder ausgetauschte Parent-Inodes und aktive lokale Parent-Moves bleiben ohne lokale Mutation fail-closed.

## Verifikation

Fokussierte Tests prüfen zwei Geräte mit demselben Parent und divergenten Zielnamen, einen abhängigen Edit, Crash nach Evakuierung, Neustart, kanonische Original-ID am Gewinnernamen, sichtbare neue Konflikt-ID, aktive A/B-Konvergenz und kalten C-Bootstrap. Ein Negativtest verschiebt den Parent lokal und bestätigt unveränderte Bytes sowie das Ausbleiben einer lokalen Konfliktordner-Publikation.

## Folgen

Divergente Note-Moves konvergieren nun für Root-Notes und für verifizierbare Moves innerhalb derselben Parent-ID. Divergente Moves zwischen verschiedenen Parent-IDs bleiben separat, weil der kanonische Konfliktsnapshot keinen vollständigen authentifizierten Pfad beider Ancestries trägt.
