# ADR 0017: Crash-sichere Materialisierung konkurrierender Notiz-Updates

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

Nach ADR 0015 kennt der Client den authentifizierten kanonischen Zustand zum Konfliktzeitpunkt; ADR 0016 reserviert den synchronisierten Bereich `_Konflikte/Wiederhergestellt`. Für `base_revision_mismatch` bei Notiz-Updates müssen die lokale und die kanonische Fassung ohne automatischen Merge erhalten bleiben. Die Materialisierung darf weder den Pull der Gewinnerfassung vorwegnehmen noch spätere lokale Bytes still verwerfen.

## Entscheidung

Schema v6 ergänzt monotone Journale für die lokale, nonce-/inode-gebundene Erstveröffentlichung der beiden reservierten Ordner und für einzelne Konfliktmaterialisierungen. Vor jeder sichtbaren Kopie werden eine neue UUIDv7, der vollständige deterministische Zielname, Quell- und Kopie-Hash sowie ein technischer Stagingpfad unveränderlich persistiert. Der Dateiname enthält die vollständige Operations-ID und reserviert die portable Längengrenze.

Unterstützt wird zunächst ausschließlich `base_revision_mismatch` für ein aktives Note-Update mit vollständigem kanonischem Notizzustand. Die letzte beim Beginn der Materialisierung beobachtete lokale Fassung wird hashadressiert technisch gesichert und mit neuer Notiz-ID sowie Herkunftsmetadaten versehen. Danach gilt der Konflikt für den kanonischen Pull als freigegeben, die sichtbare Kopie bleibt jedoch technisch gestaged.

Die Kopie wird erst veröffentlicht, wenn sowohl die reservierte `Wiederhergestellt`-Folder-ID als auch die kanonische Revision des Originalobjekts als dauerhafte Baselines bestätigt sind und Pfad, Parent, Hash und Bytes der Originalnotiz exakt dem kanonischen Zustand entsprechen. Erst nach sichtbarer exklusiver Veröffentlichung, Reconcile und erneuter kanonischer Prüfung wird die Materialisierung abgeschlossen. Abhängige Outbox-Operationen werden rekursiv superseded; die neue Konfliktnotiz wird als normale synchronisierte Create-Operation erfasst.

App-interne Save-/Move-/Delete-Aktionen auf der Quellnotiz sind zwischen technischem Staging und Abschluss gesperrt. Externe Änderungen werden vor Abschluss durch die erneute Hashprüfung erkannt und bleiben sichtbar; in diesem Fall schlägt der Abschluss geschlossen fehl. Nicht unterstützte Konfliktcodes und Mutationstypen bleiben weiterhin gezielt blockierend.

## Folgen

Konkurrierende Note-Updates konvergieren ohne Text-Merge zu einer kanonischen Originalnotiz und einer sichtbaren, synchronisierten Konfliktkopie. Abstürze vor oder nach Ordnerpublikation, technischem Staging, kanonischem Pull, sichtbarer Kopie und SQLite-Abschluss sind wiederholbar. Technische Stagingdateien werden derzeit nicht automatisch garbage-collected; begrenzte Cleanup-/Retention-Logik folgt separat.
