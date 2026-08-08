# ADR 0025: Sichere Bereinigung evakuierter Konfliktbytes

- Status: Angenommen
- Datum: 2026-08-09

## Kontext

Create-/Move-Pfadkollisionen und `object_missing` für Update/Move evakuieren die exakten ursprünglichen Markdown-Bytes nach `.remember/trash/conflicts`. Nach sichtbarer und indexgebundener Veröffentlichung der transformierten Konfliktnotiz würden diese vollständigen technischen Duplikate ohne Cleanup dauerhaft wachsen. Ein pfadbasiertes `unlink` kann auf POSIX zwischen Inode-Prüfung und Löschung jedoch eine konkurrierende Ersetzung treffen.

## Entscheidung

Die bestehende abgeschlossene Konfliktbereinigung aus ADR 0018 umfasst nun auch den deterministischen Evakuierungs-Trash für exakt autorisierte Kombinationen: Create/Move mit `path_collision` sowie Update/Move mit `object_missing`. Vorher wird weiterhin die sichtbare Konfliktnotiz über UUID, Indexhash, tatsächliche Bytes und Frontmatter geprüft.

Darwin/Linux verschieben technische Dateien zunächst auf den deterministischen `.cleanup`-Namen und öffnen ihn mit `O_NOFOLLOW|O_RDWR`. Materialisierungsstaging muss Modus `0600`, eine evakuierte kanonische Notiz Modus `0644` besitzen. Größe und SHA-256 werden am geöffneten Inode validiert; unmittelbar vor der Destruktion muss der Pfadeintrag weiterhin dieselbe Device-/Inode-Kombination besitzen.

Da POSIX portabel kein Unlink-by-handle anbietet, werden validierte Inhalte durch `ftruncate` am geöffneten Descriptor gelöscht und anschließend Datei sowie Parent-Verzeichnis fsynct. Eine nach der Prüfung ausgetauschte Pfaddatei wird dadurch nicht gelöscht; ausschließlich der bereits geöffnete validierte Inode verliert seine Bytes. Der leere `.cleanup`-Sentinel bleibt als harmlose idempotente Crash-Evidenz bestehen. Wiederholungen akzeptieren ihn nur bei weiterhin fehlendem Originalpfad, identischem geöffnetem/Pfad-Inode und erfolgreichem Descriptor-/Directory-`fsync`.

Windows bleibt bis zur handle-sicheren Reparse-Point-Implementierung fail-closed.

## Folgen

Abgeschlossene Konflikte hinterlassen keine vollständigen technischen Evakuierungs- oder Transformationsduplikate. Kleine leere Sentinels können verbleiben, enthalten aber keine Benutzerinhalte. Abstürze vor oder nach Truncation und konkurrierende Pfadersetzungen führen weder zu Datenverlust fremder Dateien noch zu einer unsicheren Cleanup-Markierung.
