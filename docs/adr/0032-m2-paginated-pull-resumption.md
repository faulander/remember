# ADR 0032: Dauerhafte Wiederaufnahme zwischen Pull-Seiten

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Ein Vordergrund-Sync zieht höchstens 100 Changes pro HTTP-Seite. Der Cursor darf erst nach vollständigem, verifiziertem Apply einer Seite fortschreiten. Ein Netzwerkfehler vor der Folgeseite muss nach Clientneustart am bestätigten Seitencursor fortsetzen und darf weder bei Cursor null beginnen noch eine nur teilweise angewendete Seite überspringen.

## Entscheidung

Der produktionsnahe Mehrgeräte-Test erzeugt über den normalen LocalCore und die echten Blob-/Sync-Routen einen Folder und 110 unterschiedliche Notizen. Zusammen mit der bestehenden Historie erzwingt dies mindestens zwei Pull-Seiten.

Ein instrumentierter Remote-Adapter lässt die erste echte Seite vollständig passieren und unterbricht exakt den zweiten Pull. Er zeichnet auf:

- den `after`-Cursor der ersten Anfrage,
- `NextCursor` der erfolgreich angewendeten ersten Seite,
- den `after`-Cursor der unterbrochenen zweiten Anfrage.

Danach wird der lokale Client geschlossen und aus demselben Root erneut geöffnet. Ein zweiter Adapter zeichnet den ersten Pull nach dem Neustart auf. Der Test verlangt, dass sowohl die unterbrochene zweite Anfrage als auch die erste Anfrage nach Neustart exakt mit dem `NextCursor` der ersten Seite beginnen. Anschließend müssen die erste und letzte Bulk-Notiz sowie bereits vorher bestehende Tombstones konvergieren.

## Folgen

Die dauerhafte Seitengrenze ist nun gegen den vollständigen Auth-/Blob-/Sync-HTTP-Stack belegt. Der Test unterscheidet echte Cursor-Wiederaufnahme von einem unbemerkten Replay ab Cursor null. Die bestehende Obergrenze von 32 Seiten pro Vordergrundlauf bleibt unverändert; größere Rückstände benötigen weitere Vordergrundläufe.
