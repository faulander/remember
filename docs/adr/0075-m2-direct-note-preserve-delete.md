# ADR 0075: Preserve-and-delete für direkte Notes

- Status: Angenommen
- Datum: 2026-08-13

## Entscheidung

Protokoll v3 erweitert ADR 0074 auf aktive direkte Notes. Folder-Identitäten werden weiterhin unter `_Konflikte/Wiederhergestellt` geklont. Eine Note wird dagegen nicht geklont: Sie behält UUID, Name, Blob-Hash und exakte Markdown-Bytes und erhält in derselben Servertransaktion eine normale Move-Version in den geklonten Recovery-Root. Dies revidiert die pauschale Fresh-ID-/Tombstone-Regel aus ADR 0069 und folgt der Identitätssemantik aus ADR 0051.

Die Cursorfolge ist deterministisch: Recovery-Root-Create, direkte leere Folder-Clones, direkte Note-Moves, Original-Child-Folder-Deletes, Original-Root-Delete. Für `f` Folder und `n` Notes gilt `last = first + 2f + n + 1`. Server und Client versiegeln Recovery-Root-Name sowie Clone- und Note-Mappings über Counts, Ordinals, Cursor, Revisionen, Parent-IDs, Namen und Blob-Hashes, bevor die Resolution als abgeschlossen gilt. Request-Version, Actor/Device, Konfliktoperation und `known_cursor` bleiben replay-exakt gebunden; v1-Request- und v2-Response-Shape sowie v1/v2-Replay bleiben unverändert.

Vor dependency-gebundenen Child-Deletes lädt der Client authentifizierte History. Belegt sie einen Remote-Move des Root-Folders, wird exakt dieser Root-Delete als Konfliktprobe vorgezogen. Nach erfolgreicher v3-Resolution werden nur die konkret gebundenen lokalen Child-Delete-Operationen superseded. Auf dem löschenden Gerät darf ein fehlender Note-Move-Source ausschließlich über das versiegelte Note-Mapping materialisiert werden; Blob und Frontmatter-UUID werden erneut geprüft. Andere fehlende Move-Sources bleiben fail-closed.

Eine bereits persistierte vorbereitete v2-Resolution wird zuerst mit ihrer unveränderten Operations-ID wiederholt. Nur nach einer authentifizierten expliziten `preserve_delete_unavailable`-Antwort darf der Client dieselbe lokale Konfliktresolution dauerhaft auf v3 und eine frische Resolution-Operations-ID umstellen und sofort als v3 wiederholen. Timeout, Verbindungsverlust, retrybarer Fehler oder eine andere mehrdeutige Antwort dürfen diesen Übergang nicht auslösen.

Direkte Folder-Children müssen leer sein. Nested Folder, Notes in Child-Foldern, post-frontier History, nicht verfügbare oder nicht berechtigte Blobs sowie mehr als 10.000 direkte Objekte bleiben fail-closed. Windows-Apply bleibt unverändert fail-closed.
