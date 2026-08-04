# Product Requirements Document: Remember

- **Status:** Entwurf
- **Version:** 0.1
- **Zielrelease:** Öffentliche Beta
- **Produktverantwortung:** Projektteam
- **Letzte Aktualisierung:** 2026-02-17
- **Technisches Begleitdokument:** [`DESIGN.md`](./DESIGN.md)

## 1. Zweck dieses Dokuments

Dieses PRD beschreibt die fachlichen Anforderungen an **Remember**, eine Local-first-Desktop-Anwendung, die persönliche Markdown-Notizen mit leistungsfähigen Erinnerungen verbindet.

Es ist die fachliche Quelle der Wahrheit für:

- Produktziele und Nicht-Ziele,
- zentrale Nutzerabläufe,
- funktionale und nichtfunktionale Anforderungen,
- den Umfang der Entwicklungsmeilensteine,
- Akzeptanz- und Release-Kriterien.

Technische Entscheidungen und die Umsetzung der Anforderungen werden in [`DESIGN.md`](./DESIGN.md) beschrieben. Die Anforderungs-IDs dieses Dokuments bleiben über spätere Überarbeitungen stabil.

## 2. Produktvision

Remember ermöglicht Einzelpersonen, ihre persönlichen Notizen als normale Markdown-Dateien in einer echten, beliebig tief verschachtelbaren Verzeichnisstruktur zu verwalten. Mehrere eigene Windows-, Linux- und macOS-Geräte synchronisieren diese Daten über einen zentralen Server, ohne die Offline-Bearbeitung einzuschränken.

Jede Notiz kann mehrere einmalige oder komplex wiederkehrende Erinnerungen enthalten. Notiz-ID und fachliche Erinnerungsdaten bleiben im YAML-Frontmatter der Markdown-Datei portabel. Technische, gerätespezifische Synchronisationsdaten bleiben davon getrennt.

Das Produkt priorisiert:

1. den Erhalt der lokalen, benutzerkontrollierten Markdown-Dateien,
2. verlustfreie Synchronisation und sichtbare Konflikte,
3. vollständige Offline-Bearbeitung,
4. vorhersehbare Erinnerungssemantik über Zeitzonen hinweg,
5. einen schlanken und bezahlbaren Betrieb.

## 3. Zielgruppe und Nutzungskontext

### 3.1 Primäre Zielgruppe

Einzelpersonen, die:

- persönliche Notizen und Erinnerungen gemeinsam verwalten möchten,
- mehrere eigene Desktop-Geräte verwenden,
- auch ohne Internetverbindung arbeiten wollen,
- Markdown-Dateien direkt besitzen und mit externen Editoren bearbeiten möchten,
- keine Kollaborations- oder Freigabefunktionen benötigen.

### 3.2 Mandantenmodell

Ein Benutzerkonto entspricht genau einem persönlichen Datenbereich. Konten sind strikt voneinander isoliert. Ein Benutzer kann mehrere eigene Geräte registrieren.

„Mehrbenutzer“ bezeichnet den sicheren Betrieb vieler isolierter Einzelkonten, nicht gemeinsame Arbeitsbereiche.

### 3.3 Unterstützte Plattformen

- Windows
- macOS
- Linux

Für die erste Entwicklung und Plattformvalidierung steht jeweils ein reales System pro Plattform zur Verfügung.

## 4. Produktziele

### G-001 – Benutzerkontrollierte Daten

Notizen müssen als normale, extern bearbeitbare `.md`-Dateien in einer echten lokalen Verzeichnisstruktur vorliegen.

### G-002 – Local-first

Alle bereits synchronisierten Ordner, Notizen und Erinnerungen müssen ohne Serververbindung vollständig les- und bearbeitbar sein.

### G-003 – Verlustfreie Synchronisation

Konkurrierende Änderungen dürfen nicht still überschrieben werden. Unsichere Situationen müssen beide Fassungen beziehungsweise Objekte erhalten und sichtbar machen.

### G-004 – Leistungsfähige Erinnerungen

Mehrere Erinnerungen pro Notiz müssen komplexe Kalenderregeln, Ausnahmen, Endbedingungen, Schlummern und Voraberinnerungen unterstützen.

### G-005 – Plattformübergreifende Vorhersagbarkeit

Dateinamen, Pfade, Dateisystemänderungen und Erinnerungen müssen auf Windows, macOS und Linux nach denselben logischen Regeln funktionieren.

### G-006 – Schlanker öffentlicher Betrieb

Die erste öffentliche Phase muss für höchstens etwa 100 aktive Benutzer durch ein bis zwei Entwickler auf einem kostengünstigen Linux-Server betreibbar sein.

### G-007 – Nachweisbare Datenintegrität

Backups, Inhaltsversionen und Wiederherstellungen müssen technisch prüfbar sein. Fehlende oder inkonsistente Daten dürfen nicht still ignoriert werden.

## 5. Erfolgskriterien

Für die öffentliche Beta gelten mindestens folgende Qualitätsziele:

- keine bestätigten Datenverluste,
- täglich erfolgreich erzeugte und geprüfte externe Backups,
- mindestens ein erfolgreich durchgeführter vollständiger Restore vor der öffentlichen Beta,
- erfolgreiche Kern-Smoke-Tests auf Windows, macOS und Linux,
- keine offenen kritischen Sicherheits- oder Datenintegritätsfehler,
- dokumentierte bekannte Beta-Einschränkungen.

Ein früher diskutiertes Ziel von 99,5 % crashfreien Sitzungen wird erst verbindlich, wenn eine hinreichende externe Stichprobe und eine genaue Sitzungsdefinition vorliegen.

Ob vor der offenen Beta externe Pilotnutzer erforderlich sind, wird entschieden, sobald Kernfunktionen und Installationsartefakte verfügbar sind.

## 6. Kernabläufe

### 6.1 Konto erstellen

1. Der Benutzer registriert sich mit E-Mail-Adresse und Passwort.
2. Der Dienst sendet einen Verifikationslink.
3. Nach erfolgreicher Verifikation kann sich der Benutzer anmelden.
4. Das erste Gerät wird als sichtbare, widerrufbare Sitzung registriert.

### 6.2 Lokalen Datenbereich einrichten

1. Der Benutzer wählt oder erstellt ein lokales Stammverzeichnis.
2. Die Anwendung prüft Schreibbarkeit und Namensregeln.
3. Sie erstellt den versteckten technischen Index.
4. Vorhandene Markdown-Dateien und Ordner werden eingelesen.
5. Fehlende stabile IDs werden kontrolliert ergänzt.

### 6.3 Notiz erstellen und bearbeiten

1. Der Benutzer erstellt einen Ordner oder eine Notiz.
2. Die Notiz wird als normale `.md`-Datei angelegt.
3. Die Anwendung ergänzt eine stabile Notiz-ID im YAML-Frontmatter.
4. Inhalt kann in Remember oder einem externen Editor geändert werden.
5. Der Dateisystem-Watcher erkennt externe Änderungen.

### 6.4 Offline arbeiten und später synchronisieren

1. Der Benutzer bearbeitet Notizen, Ordner und Erinnerungen ohne Internet.
2. Änderungen werden lokal erfasst.
3. Nach Wiederherstellung der Verbindung überträgt die App Änderungen idempotent.
4. Der Server liefert Änderungen anderer eigener Geräte.
5. Konflikte werden nach den festgelegten verlustfreien Regeln materialisiert.

### 6.5 Konflikt bearbeiten

1. Die App zeigt Konfliktkopien und Strukturkonflikte deutlich an.
2. Herkunft, Zeitpunkt, Gerät und Konfliktgrund sind nachvollziehbar.
3. Der Benutzer vergleicht, verschiebt, vereinigt oder löscht Fassungen bewusst.
4. Die Auflösung wird wie jede andere Änderung synchronisiert.

### 6.6 Erinnerung anlegen

1. Der Benutzer wählt eine Notiz.
2. Er legt Datum, Uhrzeit und feste IANA-Zeitzone fest.
3. Optional definiert er Wiederholung, Endbedingung, Ausnahmen und Voraberinnerungen.
4. Die fachlichen Daten werden im Frontmatter gespeichert und synchronisiert.

### 6.7 Erinnerung auslösen

1. Ist die App geöffnet, ermittelt sie lokal fällige Erinnerungen.
2. Online koordiniert der Server, welches geöffnete Gerät benachrichtigt.
3. Offline darf jedes Gerät lokal benachrichtigen.
4. Der Benutzer kann erledigen, schlummern oder die Notiz öffnen.
5. Verpasste Termine werden beim nächsten Öffnen angezeigt.

### 6.8 Gerät oder Sitzung widerrufen

1. Der Benutzer öffnet die Geräte-/Sitzungsverwaltung.
2. Er sieht bekannte Geräte und aktive Sitzungen.
3. Er widerruft eine Sitzung.
4. Das betroffene Gerät darf nicht weiter mit dem Server synchronisieren.
5. Bereits vorhandene lokale Markdown-Dateien bleiben lokal erhalten.

### 6.9 Konto löschen

1. Der Benutzer authentifiziert sich erneut.
2. Alle Sitzungen werden sofort widerrufen.
3. Aktive Serverdaten werden zeitnah endgültig gelöscht.
4. Backupkopien laufen innerhalb der dokumentierten Maximalfrist aus.
5. Lokale Markdown-Dateien bleiben bestehen, bis der Benutzer sie selbst löscht.

## 7. Funktionale Anforderungen

## 7.1 Konten und Geräte

### ACC-001 – Offene Registrierung

Der Dienst muss eine offene Registrierung mit E-Mail-Adresse und Passwort anbieten.

### ACC-002 – E-Mail-Verifikation

Ein Konto darf erst nach erfolgreicher Verifikation der E-Mail-Adresse vollständig aktiviert werden.

### ACC-003 – Passwortschutz

Passwörter müssen serverseitig mit Argon2id und aktuellen, dokumentierten Parametern gehasht werden.

### ACC-004 – Kontowiederherstellung

Passwort-Reset muss über kurzlebige, einmal verwendbare Links erfolgen. Antworten dürfen nicht offenlegen, ob eine E-Mail-Adresse registriert ist.

### ACC-005 – Geräte und Sitzungen

Benutzer müssen ihre Geräte und aktiven Sitzungen sehen, benennen und widerrufen können.

### ACC-006 – Mehrere eigene Geräte

Ein Konto muss mehrere gleichzeitig registrierte Windows-, macOS- und Linux-Geräte unterstützen.

### ACC-007 – Kontolöschung

Kontolöschung muss erneute Authentifizierung verlangen, alle Sitzungen sofort widerrufen und aktive Serverdaten zeitnah endgültig entfernen.

## 7.2 Lokale Dateien und Ordner

### DATA-001 – Markdown als kanonische Notiz

Jede Notiz muss lokal als normale `.md`-Datei vorliegen.

### DATA-002 – Echte Verzeichnisstruktur

Ordner müssen als echte lokale Verzeichnisse vorliegen. Die Anwendung darf keine künstliche Verschachtelungsgrenze setzen.

Technische Pfad- und Dateisystemgrenzen bleiben zulässig.

### DATA-003 – Externe Bearbeitung

Die Anwendung muss externe Erstellung, Bearbeitung, Umbenennung, Verschiebung und Löschung erkennen und verarbeiten.

### DATA-004 – Stabile Notiz-ID

Jede synchronisierte Notiz muss eine stabile UUID im versionierten YAML-Frontmatter tragen.

### DATA-005 – Fachliche Erinnerungsdaten

Erinnerungsdefinitionen und synchronisierte fachliche Instanzzustände müssen im YAML-Frontmatter der zugehörigen Notiz gespeichert werden.

### DATA-006 – Technischer Index

Gerätespezifische Sync-Zustände, Hashes, Watcher-Daten und UUID-Pfad-Zuordnungen müssen in einem versteckten lokalen Index gespeichert werden, nicht im Frontmatter.

### DATA-007 – Ordneridentität

Jeder Ordner muss eine stabile UUID besitzen und als eigenständiges Objekt synchronisiert werden.

### DATA-008 – Leere Ordner

Leere Ordner müssen geräteübergreifend synchronisiert werden.

### DATA-009 – Indexrekonstruktion

Der technische Index muss aus lokalen Dateien und Servermetadaten kontrolliert rekonstruierbar sein. Unklare Zuordnungen dürfen nicht still geraten werden.

### DATA-010 – Frontmatter-Verträglichkeit

Die Anwendung muss unbekannte Frontmatter-Felder soweit möglich erhalten. Ungültiges YAML, inkompatible Schemaversionen und doppelte IDs müssen sichtbar behandelt werden.

## 7.3 Portable Namen

### NAME-001 – Gemeinsame Regeln

Auf allen Plattformen müssen dieselben logischen Namensregeln gelten.

### NAME-002 – Windows-Kompatibilität

Windows-reservierte Zeichen, Endzeichen und Gerätenamen müssen auf allen Plattformen verboten sein.

### NAME-003 – Unicode-Normalisierung

Namen müssen nach Unicode NFC normalisiert werden.

### NAME-004 – Case-insensitive Eindeutigkeit

Geschwisternamen müssen unabhängig von Groß-/Kleinschreibung eindeutig sein.

### NAME-005 – Längenlimits

Komponenten- und Gesamtpfade müssen konservative, versionierte Längenlimits einhalten und Reserve für Konfliktsuffixe lassen.

### NAME-006 – Externe ungültige Namen

Extern angelegte ungültige Namen dürfen nicht still verändert werden. Die App muss ein lokales Sync-Problem und einen Umbenennungsdialog anzeigen.

## 7.4 Local-first und Synchronisation

### SYNC-001 – Vollständige Offline-Bearbeitung

Vorhandene Ordner, Notizen und Erinnerungen müssen offline vollständig les- und bearbeitbar sein.

### SYNC-002 – Später Abgleich

Offline-Änderungen müssen nach Wiederherstellung der Verbindung synchronisiert werden.

### SYNC-003 – Idempotenz

Wiederholte Übertragung derselben Operation darf keine doppelten Objekte oder Zustandsänderungen erzeugen.

### SYNC-004 – Änderungslog

Der Server muss ein geordnetes, mandantenspezifisches Änderungslog bereitstellen, über das Clients seit ihrem letzten bestätigten Stand fortsetzen können.

### SYNC-005 – Tombstones

Löschungen müssen als Tombstones lange genug erhalten bleiben, damit länger offline befindliche Geräte sie zuverlässig erhalten.

### SYNC-006 – Bearbeitungskonflikt

Wurde dieselbe Notiz seit dem letzten gemeinsamen Stand konkurrierend geändert, müssen beide Fassungen erhalten bleiben. Eine Fassung wird als klar markierte Konfliktkopie materialisiert.

### SYNC-007 – Kein automatischer Text-Merge

V1 darf konkurrierende Textänderungen nicht automatisch zusammenführen.

### SYNC-008 – Löschen gegen Bearbeiten oder Verschieben

Die Löschung am ursprünglichen Pfad bleibt wirksam. Die konkurrierende Fassung muss verlustfrei unter einem reservierten Wiederherstellungsbereich gespeichert werden.

### SYNC-009 – Pfadkollision

Beanspruchen zwei Objekte denselben Zielnamen, müssen beide im Zielordner erhalten bleiben. Eine deterministische Regel vergibt den Originalnamen; das andere Objekt erhält einen sichtbaren Konfliktsuffix.

### SYNC-010 – Konfliktherkunft

Konfliktobjekte müssen Ursprung, ursprünglichen Zielnamen, beteiligte Geräte/Versionen und Konfliktgrund nachvollziehbar speichern.

### SYNC-011 – Externe Ordneroperationen

Laufende externe Ordnerbewegungen müssen durch den Watcher stabilen Ordner-UUIDs zugeordnet werden. Unklare Änderungen nach App-Stillstand müssen konservativ als Konflikt behandelt werden.

### SYNC-012 – Fortsetzbarer Sync

Ein einzelner Konflikt darf nicht die gesamte Synchronisation unbeteiligter Objekte blockieren.

### SYNC-013 – Integritätsfehler

Fehlende referenzierte Inhalte, ungültige Hashes oder unauflösbare Objektzustände müssen sichtbar fehlschlagen und dürfen nicht still verworfen werden.

## 7.5 Erinnerungen

### REM-001 – Mehrere Erinnerungen

Eine Notiz muss mehrere unabhängig identifizierbare Erinnerungen unterstützen.

### REM-002 – Einmalige Termine

Eine Erinnerung muss einen einmaligen Termin mit Datum, Uhrzeit und IANA-Zeitzone unterstützen.

### REM-003 – Kalenderbasierte Wiederholungen

Eine Erinnerung muss komplexe kalenderbasierte Wiederholungsregeln unterstützen, einschließlich täglicher, wöchentlicher, monatlicher und jährlicher Muster.

### REM-004 – Endbedingungen

Wiederholungen müssen durch Enddatum oder maximale Anzahl von Instanzen begrenzt werden können.

### REM-005 – Ausnahmen

Einzelne Instanzen müssen ausgelassen oder abweichend behandelt werden können.

### REM-006 – Voraberinnerungen

Eine Erinnerung muss eine oder mehrere Benachrichtigungen vor dem eigentlichen Termin unterstützen.

### REM-007 – Schlummern

Eine fällige Instanz muss um vordefinierte oder frei gewählte Zeit verschoben werden können.

### REM-008 – Erledigen

Der Benutzer muss eine Instanz als erledigt markieren können, ohne die gesamte Wiederholungsserie zu löschen.

### REM-009 – Feste IANA-Zeitzone

Jede Erinnerung muss eine feste IANA-Zeitzone speichern. Wiederholungen bleiben an die lokale Uhrzeit dieser Zone gebunden und folgen deren Sommerzeitregeln.

### REM-010 – Reisen

Eine Änderung der Gerätezeitzone darf den tatsächlichen Termin nicht verschieben. Nur die Darstellung darf in die aktuelle Gerätezone umgerechnet werden.

### REM-011 – App-geöffnete Zustellung

Ist mindestens ein benachrichtigungsfähiger App-Prozess geöffnet, muss die Anwendung die native Zustellung einer fälligen Erinnerung versuchen. Online wird ein nicht bestätigter Zustellversuch nach Lease-Ablauf auf einem erreichbaren Gerät wiederholt; seltene Duplikate sind zulässig. Verweigert das Betriebssystem Benachrichtigungen oder schlagen alle Adapter fehl, muss die Erinnerung sichtbar in der geöffneten App beziehungsweise beim nächsten Öffnen als verpasst erscheinen. Eine Zustellung bei vollständig geschlossener App ist nicht erforderlich.

### REM-012 – Verpasste Erinnerungen

Beim nächsten Öffnen muss die App verpasste Erinnerungen nach einer dokumentierten Regel anzeigen.

### REM-013 – Online-Gerätewahl

Sind mehrere Apps online geöffnet, muss der Server ein Gerät für die Benachrichtigung auswählen.

### REM-014 – Offline-Zustellung

Ohne Serververbindung darf jedes Gerät aufgrund seines lokalen Stands benachrichtigen. Kurzzeitige Duplikate werden akzeptiert.

### REM-015 – Zustandsabgleich

Erledigen, Schlummern und Ausnahmen müssen als fachliche Zustände zwischen Geräten synchronisiert und bei konkurrierenden Offline-Aktionen verlustfrei behandelt werden.

## 7.6 Sicherheit und Datenschutz

### SEC-001 – TLS

Sämtliche Client-Server-Kommunikation muss ausschließlich über TLS erfolgen.

### SEC-002 – Mandantentrennung

Jeder API-, Datenbank-, Blob- und Änderungslogzugriff muss serverseitig auf den authentifizierten Benutzer eingeschränkt werden.

### SEC-003 – Verschlüsselung ruhender Serverdaten

Server-Datenträger und externe Backups müssen verschlüsselt sein.

### SEC-004 – Kein E2E in V1

Das Backend darf Inhalte autorisiert im Klartext verarbeiten. Diese Eigenschaft muss transparent dokumentiert werden.

### SEC-005 – Least Privilege

Administrative und betriebliche Zugriffe müssen minimiert, kontrolliert und protokolliert werden.

### SEC-006 – Sichere lokale Sitzungstokens

Langfristige Anmeldedaten müssen in der sicheren plattformspezifischen Schlüsselablage gespeichert werden, soweit verfügbar.

### SEC-007 – Missbrauchsschutz

Registrierung, Anmeldung, Verifikation und Recovery müssen Rate Limits und Schutz gegen Enumeration und automatisierten Missbrauch besitzen.

### SEC-008 – Lokale Daten nach Kontolöschung

Die App darf normale lokale Markdown-Dateien bei einer Kontolöschung nicht automatisch entfernen.

### SEC-009 – Backupauslauf gelöschter Konten

Daten gelöschter Konten müssen mit Ablauf der dokumentierten maximalen Backupaufbewahrung verschwinden.

## 7.7 Betrieb, Backup und Wiederherstellung

### OPS-001 – Zielgröße

Die erste Backend-Variante muss bis zu etwa 100 aktive Benutzer auf einem Linux-Server unterstützen.

### OPS-002 – Tägliches externes Backup

Mindestens einmal täglich muss ein verschlüsseltes Backup auf ein administrativ oder physisch getrenntes Ziel geschrieben werden.

### OPS-003 – Backupprüfung

Jedes Backup muss automatisiert auf Datenbanklesbarkeit, referenzierte Blobs und Hashintegrität geprüft werden.

### OPS-004 – Restore-Test

Vor der öffentlichen Beta muss mindestens ein vollständiger Restore erfolgreich durchgeführt werden. Restore-Tests sind danach regelmäßig zu wiederholen.

### OPS-005 – Keine feste RTO

Für Pilot und offene Beta besteht keine verbindliche Wiederherstellungszeit. Der Dienst muss diese Einschränkung transparent dokumentieren.

### OPS-006 – Lokale Weiterarbeit

Ein Serverausfall darf den Zugriff auf bereits lokale Daten und deren Bearbeitung nicht verhindern.

### OPS-007 – Monitoring

Der Betrieb muss mindestens Verfügbarkeit, Fehlerquoten, Sync-Ergebnisse, Backupstatus, Speicherverbrauch und Integritätsfehler überwachen.

### OPS-008 – Inhaltsintegrität

Jede serverseitige Inhaltsversion muss durch SHA-256 adressiert und verifizierbar sein.

## 7.8 Diagnose und Telemetrie

### TEL-001 – Pilotdiagnose

Pilotnutzer müssen einer datensparsamen technischen Diagnose vorab zustimmen.

### TEL-002 – Zulässige Felder

Erfasst werden dürfen pseudonyme Geräte-/Sitzungs-IDs, App-/OS-Version, Crash-Stacktraces, Sync-Ergebnis, Dauer, Fehlercodes und Objektanzahlen.

### TEL-003 – Verbotene Felder

Notizinhalte, Dateinamen, Pfade, Frontmatter-Werte und Erinnerungstexte dürfen niemals erhoben werden.

### TEL-004 – Redigierung

Crash- und Fehlerdaten müssen auf versehentlich enthaltene lokale Pfade oder persönliche Werte redigiert werden.

### TEL-005 – Offene Beta

Clientdiagnose muss in der offenen Beta transparent abschaltbar sein. Servermetriken dürfen aggregiert weitergeführt werden.

### TEL-006 – Aufbewahrung

Diagnosedaten müssen eine dokumentierte, begrenzte Aufbewahrung und Zugriffskontrolle besitzen.

## 7.9 Packaging und Updates

### REL-001 – Drei Plattformartefakte

Für Windows, macOS und Linux müssen installierbare beziehungsweise entpackbare Client-Artefakte bereitgestellt werden.

### REL-002 – Frühe unsignierte Builds

Die erste öffentliche Phase darf unsignierte ZIP-/AppImage-Artefakte verwenden. Warnungen und Installationsschritte müssen deutlich dokumentiert werden.

### REL-003 – Prüfsummen

Jedes CI-Release-Artefakt muss eine veröffentlichte SHA-256-Prüfsumme besitzen.

### REL-004 – Signiertes Release-Manifest

Prüfsummen, Versionen und Protokollkompatibilität müssen in einer separat kryptografisch signierten Manifestdatei veröffentlicht werden.

### REL-005 – Versionsendpunkt

Die App muss beim Start einen TLS-geschützten Versionsendpunkt prüfen und auf neue Versionen hinweisen können.

### REL-006 – Mindestversion

Der Server muss kompromittierte oder protokollinkompatible Clientversionen von weiterer Serverkommunikation ausschließen können, ohne lokale Dateien zu sperren.

### REL-007 – Manuelle Updates

Updates werden in der ersten öffentlichen Phase manuell installiert.

### REL-008 – Spätere Releasehärtung

Code-Signing, macOS-Notarisierung und sichere automatische Updates bleiben spätere Produktziele.

## 8. Nichtfunktionale Anforderungen

### NFR-001 – Keine stille Datenvernichtung

Kein Fehlerpfad darf Benutzerdaten oder konkurrierende Fassungen still verwerfen.

### NFR-002 – Determinismus

Konfliktentscheidungen müssen auf stabilen IDs und Operationsordnungen basieren, nicht auf unsicheren Geräteuhren.

### NFR-003 – Reproduzierbarkeit

Dateisystem-, Sync- und Reminder-Zustandsübergänge müssen durch automatisierte Tests reproduzierbar sein.

### NFR-004 – Rekonstruierbarkeit

Gerätespezifischer technischer Zustand darf nicht die einzige Quelle fachlicher Daten sein.

### NFR-005 – Migrationsfähigkeit

Frontmatter-, Sync-Protokoll- und Datenbankschemata müssen versioniert und kontrolliert migrierbar sein.

### NFR-006 – Bedienbarkeit von Konflikten

Konflikte müssen verständlich, sichtbar und ohne technische Dateisystemkenntnisse auflösbar sein.

### NFR-007 – Ressourcenrahmen

Die Architektur muss durch ein bis zwei Entwickler betreibbar bleiben und darf keine unnötigen verteilten Dienste voraussetzen.

## 9. Scope und Meilensteine

## 9.1 Meilenstein 1 – Lokaler Datenkern

Enthalten:

- echte Markdown-Dateien,
- verschachtelte Ordner,
- stabile Notiz- und Ordner-UUIDs,
- YAML-Frontmatter-Grundschema,
- versteckter lokaler Index,
- portable Namensvalidierung,
- Dateisystem-Watcher,
- kontrollierter Reconcile nach App-Stillstand,
- lokale Konflikt- und Fehleranzeigen.

Nicht enthalten:

- Server,
- Konten,
- Mehrgeräte-Sync,
- vollständiges Reminder-System.

## 9.2 Meilenstein 2 – Identität und Synchronisation

Enthalten:

- Registrierung, Verifikation und Anmeldung,
- Geräte/Sitzungen,
- Backend-Grundstruktur,
- Änderungslog und Tombstones,
- Mehrgeräte-Sync,
- versionierte Inhaltsblobs,
- vollständige Konfliktmatrix für Inhalt und Struktur,
- Offline-/Wiederaufnahme-Tests.

## 9.3 Meilenstein 3 – Erinnerungen

Enthalten:

- vollständiges Reminder-Schema,
- Kalenderwiederholungen,
- IANA-Zeitzonen und DST-Regeln,
- Ausnahmen und Endbedingungen,
- Voraberinnerungen,
- Schlummern und Erledigen,
- verpasste Termine,
- native Benachrichtigungen,
- Online-Gerätewahl und Offline-Verhalten.

## 9.4 Meilenstein 4 – Öffentliche Betriebsreife

Enthalten:

- Recovery,
- vollständige Geräte-/Sitzungsverwaltung,
- Kontolöschung,
- Missbrauchsschutz,
- Backup und Restore,
- Telemetrie und Monitoring,
- Release-Artefakte für drei Plattformen,
- Prüfsummen und signiertes Manifest,
- Versionsendpunkt und Mindestversion,
- Plattform-Smoke-Tests,
- Datenschutz- und Betriebsdokumentation.

## 10. Akzeptanzkriterien nach Meilenstein

### M1-AC-001

Eine extern erstellte, bearbeitete, verschobene und gelöschte Markdown-Datei wird auf allen drei Plattformen korrekt erkannt, ohne andere Dateien zu verändern.

### M1-AC-002

Leere und nichtleere Ordner behalten bei App-internen sowie laufend beobachteten externen Verschiebungen ihre UUID.

### M1-AC-003

Ungültige portable Namen werden sichtbar gemeldet und nicht still verändert.

### M1-AC-004

Nach Löschung und Rekonstruktion des technischen Index bleiben fachliche Daten erhalten; unklare Identitäten werden als Konflikt angezeigt.

### M2-AC-001

Zwei Geräte können unabhängig offline arbeiten und nach Wiederverbindung ohne stillen Datenverlust konvergieren.

### M2-AC-002

Die definierte Matrix für Bearbeiten/Bearbeiten, Löschen/Bearbeiten, Verschieben/Löschen und Pfadkollisionen besteht automatisierte Tests.

### M2-AC-003

Wiederholte Requests und Sync-Wiederaufnahme erzeugen keine doppelten Operationen oder Objekte.

### M2-AC-004

Ein fehlender oder hashinkonsistenter Blob wird als Integritätsfehler erkannt und alarmiert.

### M3-AC-001

Kalenderregeln liefern auf allen Plattformen dieselben Instanzen für dieselbe TZDB-Version und denselben Zustand.

### M3-AC-002

DST-Sprungfälle, doppelte Uhrzeiten, Monatsenden, Ausnahmen und Endbedingungen bestehen dokumentierte Testvektoren.

### M3-AC-003

Online wählt der Server genau ein Gerät pro Zustell-Lease aus. Seltene doppelte Benachrichtigungen nach Lease-Ablauf oder Verbindungsabbruch sind zulässig und werden in Partitionstests nachgewiesen; offline darf jedes Gerät benachrichtigen.

### M3-AC-004

Erledigen und Schlummern werden synchronisiert; widersprüchliche Offline-Aktionen führen nicht zum stillen Verlust einer Benutzerentscheidung.

### M3-AC-005

Vor Abschluss des Meilensteins ist eine versionierte Regel für verpasste Instanzen festgelegt und durch Testvektoren abgedeckt. Sie definiert mindestens Rückblickgrenze, Aggregation langer Serien, Reihenfolge, maximale Einzelanzeigen und den resultierenden benutzersichtbaren Zustand.

### M4-AC-001

Registrierung, Verifikation, Login, Recovery, Sitzungswiderruf und Kontolöschung bestehen End-to-End-Tests.

### M4-AC-002

Ein tägliches externes Backup wird automatisiert erzeugt und vollständig geprüft.

### M4-AC-003

Ein vollständiger Restore auf eine leere Umgebung stellt Datenbank und alle referenzierten Blobs nachweislich wieder her.

### M4-AC-004

Windows-, macOS- und Linux-Artefakte bestehen je einen Installations-, Datei-Watcher-, Sync- und Benachrichtigungs-Smoke-Test.

### M4-AC-005

Diagnoseereignisse enthalten in automatisierten Datenschutztests keine verbotenen Inhalts-, Pfad- oder Frontmatter-Daten.

## 11. Ausdrückliche Nicht-Ziele

Nicht Bestandteil von V1 beziehungsweise der ersten öffentlichen Phase sind:

- Teilen, Freigaben, Einladungen oder Kollaboration,
- gemeinsame Bereiche und Rollen,
- Ende-zu-Ende-Verschlüsselung,
- MFA,
- automatischer Text-Merge,
- Erinnerungszustellung bei vollständig geschlossener App,
- E-Mail- oder externe Push-Erinnerungen,
- erzwungenes Löschen lokaler Markdown-Dateien bei Kontolöschung,
- Microservices, Message Broker oder horizontale Skalierung,
- PostgreSQL oder S3-kompatibler Objektspeicher,
- feste Wiederherstellungszeit des Serverdienstes,
- Code-Signing, macOS-Notarisierung und Auto-Updates,
- plattformspezifisch unterschiedliche logische Dateinamen,
- Erhebung von Notizinhalten oder identifizierenden Pfaden in der Diagnose.

## 12. Rollout

### 12.1 Interne Entwicklung

Die vier Meilensteine werden nacheinander risikogestuft umgesetzt. Jeder Meilenstein benötigt automatisierte Tests und reale Smoke-Tests auf den verfügbaren Plattformen.

### 12.2 Entscheidungspunkt für externe Piloten

Sobald Kernfunktionen und Installationsartefakte verfügbar sind, wird entschieden, ob vor der offenen Beta externe Pilotnutzer benötigt werden. Kriterien sind insbesondere:

- beobachtete Stabilität,
- Installationshürden unsignierter Builds,
- verbleibende Unsicherheit der Konflikt-UX,
- Hardware-/OS-Abdeckung,
- Aussagekraft der vorhandenen Telemetrie.

### 12.3 Offene Beta

Die offene Beta ist auf etwa 100 aktive Nutzer begrenzt. Bekannte Einschränkungen, insbesondere unsignierte Builds, manuelle Updates, fehlende Hintergrundzustellung und fehlende RTO, werden deutlich kommuniziert.

## 13. Bewusst vertagte Entscheidungen

Folgende Entscheidungen werden vor oder innerhalb des zuständigen Meilensteins spezifiziert:

- exaktes versioniertes Frontmatter- und Reminder-Schema,
- Verhalten bei ungültigem YAML und doppelten IDs,
- exakte Komponenten- und Pfadlängenlimits,
- konkrete Reminder-Instanzzustände und DST-Sprungregeln,
- Lease-Zeitfenster und widersprüchliche Offline-Aktionen,
- Tokenrotation, Rate Limits und Audit-Aufbewahrung,
- Backupgenerationen, Restore-Testfrequenz und Blob-GC-Schutzfrist,
- genaue Definition von bestätigtem Datenverlust,
- Ersatz-Solo-Stabilitätsgate und externe Pilotentscheidung,
- Release-Schlüsselrotation und Verhalten kompromittierter Clients.

## 14. Offene Produktfragen

Vor der jeweiligen Umsetzung sind insbesondere zu beantworten:

1. Wie werden mehrere verpasste Instanzen einer langen Wiederholungsserie dargestellt?
2. Welche UX erhält ein Benutzer bei ungültigem oder teilweise lesbarem Frontmatter?
3. Wie lange bleiben Tombstones und Konfliktherkunft erhalten?
4. Welche Mindestinformationen zeigt die Konfliktansicht ohne sensible Inhalte zu protokollieren?
5. Welche Grenzen gelten für Anzahl und Komplexität von Erinnerungen pro Notiz?
6. Welche Installationshinweise sind für Gatekeeper, SmartScreen und Linux-Desktops nötig?

## 15. Änderungsregeln

- Neue Anforderungen erhalten neue IDs; bestehende IDs werden nicht neu verwendet.
- Entfallene Anforderungen bleiben als „entfallen“ dokumentiert.
- Änderungen an Konflikt-, Sicherheits- oder Datenintegritätsregeln benötigen eine explizite Designbewertung.
- Änderungen an Zielumfang oder Nicht-Zielen müssen sowohl PRD als auch Design-Dokument aktualisieren.
