# ADR 0018: Identitätsgebundene Bereinigung technischer Konfliktkopien

- Status: Angenommen
- Datum: 2026-08-08

## Kontext

ADR 0017 hält die transformierten Konfliktbytes bis nach sichtbarer Veröffentlichung und atomarem Materialisierungsabschluss unter `.remember/conflicts/materializations` vor. Ohne wiederaufnehmbare Bereinigung würden diese privaten Duplikate unbegrenzt wachsen. Eine pfadbasierte Löschung dürfte jedoch weder eine konkurrierende Ersetzung entfernen noch nach Verlust der sichtbaren Konfliktkopie die letzte erhaltene Fassung löschen.

## Entscheidung

Schema v7 ergänzt pro abgeschlossener Materialisierung einen unveränderlichen Cleanup-Zeitpunkt. Er darf ausschließlich nach `completed` gesetzt und niemals zurückgenommen oder verändert werden.

Vor der technischen Bereinigung prüft der Client erneut, dass die sichtbare Konfliktnotiz im Index mit erwarteter Konflikt-UUID und SHA-256 gebunden ist und dass die tatsächlich gelesenen Markdown-Bytes dieselbe UUID und denselben Hash tragen. Erst danach wird die private Stagingdatei descriptor-relativ auf einen deterministischen Cleanup-Namen verschoben.

Darwin/Linux öffnen diesen Cleanup-Eintrag mit `O_NOFOLLOW`, prüfen Modus, Größe und SHA-256 am geöffneten Inode und vergleichen unmittelbar vor `unlinkat` dessen Device/Inode erneut mit dem Pfadeintrag. Eine konkurrierende Ersetzung des ursprünglichen oder des Cleanup-Pfads bleibt erhalten und lässt den Vorgang geschlossen fehlschlagen. Verwaiste, bereits fehlende technische Bytes gelten als idempotent bereinigt. Windows bleibt bis zum handle-sicheren Reparse-Point-Schutz geschlossen.

Erst nach erfolgreichem Dateisystem-Cleanup wird `cleaned_at_ms` gesetzt. Ein Absturz davor wiederholt die descriptor-gebundene Bereinigung; ein Absturz danach findet keinen offenen Cleanup mehr.

## Folgen

Abgeschlossene Update/Update-Konflikte hinterlassen keine vollständigen technischen Markdown-Duplikate. Sichtbare Konfliktkopien bleiben die kanonischen lokalen Dateien und normale synchronisierte Objekte. Fehlende oder ersetzte sichtbare Kopien blockieren die Bereinigung bewusst, damit technische Bytes nicht als letzte erhaltene Fassung verloren gehen.
