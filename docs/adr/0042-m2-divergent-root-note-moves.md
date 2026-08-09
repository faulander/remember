# ADR 0042: Divergente konkurrierende Root-Note-Moves

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Zwei Geräte können dieselbe Root-Note offline auf unterschiedliche Root-Namen verschieben. Der erste Move gewinnt serverseitig. Der zweite erhält `base_revision_mismatch` mit kanonischem Note-Zustand. Lokale Folge-Edits können bereits von diesem verlierenden Move abhängen und dürfen nicht verloren gehen.

Der kanonische Konfliktzustand enthält Parent-ID und Namen, aber nicht die vollständigen Revisionen aller Folder-Ancestors. Eine automatische Pfadableitung für nicht-root-basierte Moves könnte deshalb auf einer veralteten lokalen Ancestry beruhen.

## Entscheidung

Automatische Materialisierung ist zunächst strikt auf divergente Root-Moves beschränkt:

- Mutation ist Note-Move mit `base_revision_mismatch`.
- Kanonischer Zustand ist eine nicht gelöschte Note mit gültigem Blobhash.
- Kanonische Revision ist strikt höher als die lokale Basisrevision.
- Vorgeschlagener und kanonischer Parent sind beide Root.
- Die Namen unterscheiden sich.

Die verlierende lokale Fassung verwendet die bestehende crash-resumierbare Move-Konfliktmaterialisierung:

1. Aktuelle lokale Bytes einschließlich abhängiger Edits werden hashgebunden gestaged.
2. Abhängige, noch nicht gesendete Intents werden erst nach dauerhafter Staging-Zeile superseded.
3. Der lokale Zielpfad wird exakt evakuiert.
4. Der authentifizierte kanonische Blob wird unter der ursprünglichen Note-ID am kanonischen Root-Namen hergestellt.
5. Die lokale Fassung wird unter neuer UUID in `_Konflikte/Wiederhergestellt` veröffentlicht und synchronisiert.
6. Technische Staging- und Evakuierungsbytes werden erst nach verifizierter sichtbarer Kopie entfernt.

Move-Konflikte unter Foldern bleiben fail-closed, solange die aktuelle Parent-Ancestry nicht authentifiziert ableitbar ist. Äquivalente Note-Moves zum exakt gleichen Ziel bleiben ebenfalls separat, da sie keine Konfliktkopie benötigen.

## Verifikation

Der Mehrgerätetest deckt divergente Root-Namen, einen abhängigen lokalen Edit, Absturz direkt nach Evakuierung, Neustart, kanonische Wiederherstellung, neue Konflikt-UUID, zweites Gerät und vollständige technische Bereinigung ab. Ein negativer Test injiziert eine nicht fortschreitende kanonische Revision und beweist, dass weder Evakuierung noch Pfadmutation stattfindet.

## Folgen

Die häufige Root-Rename/Rename-Zelle konvergiert verlustfrei. Unsichere Ancestry-Heuristiken werden nicht eingeführt; nicht-root-basierte Varianten benötigen zusätzliche authentifizierte Parent-Pfad-Projektionen oder einen Pull-vor-Evakuierung-Zustandsautomaten.
