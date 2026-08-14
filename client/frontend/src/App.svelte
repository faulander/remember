<script lang="ts">
  import { onMount, tick } from 'svelte';
  import {
    CloseRoot, CreateFolder, CreateNote, DeleteNote, InitializeRoot, ListSessions, Login, Logout, MoveNote, OpenRoot,
    ReadNote, Refresh, RegisterAccount, RenameCurrentDevice, RestoreSession, RevokeDevice, RevokeSession, SaveNote,
    SelectRoot, NormalizeTags, SyncNow, VerifyEmail,
  } from '../wailsjs/go/main/DesktopApp';
  import { EventsOn } from '../wailsjs/runtime/runtime';
  import type { main } from '../wailsjs/go/models';
  import { renderMarkdown } from './markdown';
  import { hasPostSubmitChanges, saveResponseAction } from './editor-state';
  import { filterByTag, tagFilters, validTagSelection } from './tags';
  import { buildNoteTree, flattenNoteTree } from './note-tree';
  import { acceptsVersion } from './ordering';
  import {
    resolvedScheme, storedMode, storedTheme,
    type AppearanceMode, type Theme,
  } from './appearance';

  type StateEvent = { generation: number; revision: number; state?: main.ClientState; error?: string };
  type ViewMode = 'edit' | 'preview' | 'split';
  type ManagedDevice = { deviceId: string; deviceName: string; current: boolean; sessions: main.ManagedSessionView[] };
  type AuthMode = 'login' | 'register' | 'verify';

  const modes: { value: AppearanceMode; label: string }[] = [
    { value: 'system', label: 'System' }, { value: 'light', label: 'Hell' }, { value: 'dark', label: 'Dunkel' },
  ];
  const themes: { value: Theme; label: string }[] = [
    { value: 'remember', label: 'Remember' }, { value: 'nord', label: 'Nord' },
    { value: 'dracula', label: 'Dracula' }, { value: 'solarized', label: 'Solarized' },
    { value: 'catppuccin', label: 'Catppuccin' },
  ];

  let state: main.ClientState | null = null;
  let selection: main.RootSelection | null = null;
  let activeGeneration = 0;
  let activeRevision = 0;
  let busy = false;
  let error = '';
  let notice = '';
  let note: main.NoteView | null = null;
  let body = '';
  let tags: string[] = [];
  let baselineBody = '';
  let baselineTags: string[] = [];
  let tagInput = '';
  let selectedTag = '';
  let collapsedFolders: string[] = [];
  let collapsedRoot = '';
  let stale: '' | 'changed' | 'deleted' = '';
  let viewMode: ViewMode = 'split';
  let createOpen = false;
  let createFolder = '';
  let createName = '';
  let folderOpen = false;
  let folderParent = '';
  let folderName = '';
  let contextMenu: { x: number; y: number; folder: string } | null = null;
  let contextMenuElement: HTMLDivElement | null = null;
  let contextMenuTrigger: HTMLElement | null = null;
  let moveOpen = false;
  let moveFolder = '';
  let moveName = '';
  let deleteOpen = false;
  let syncSequence = 0;
  let appearanceMode: AppearanceMode = 'system';
  let theme: Theme = 'remember';
  let session: main.SessionView | null = null;
  let loginOpen = false;
  let authMode: AuthMode = 'login';
  let loginServer = 'http://127.0.0.1:8080';
  let loginEmail = '';
  let loginPassword = '';
  let loginPasswordConfirm = '';
  let verificationToken = '';
  let loginDevice = 'Remember Desktop';
  let accountOpen = false;
  let managedSessions: main.ManagedSessionView[] = [];
  let currentDeviceName = '';

  $: notes = state?.objects.filter((object) => object.type === 'note') ?? [];
  $: folders = state?.objects.filter((object) => object.type === 'folder').map((object) => object.relativePath).sort() ?? [];
  $: allTags = tagFilters(notes);
  $: selectedTag = validTagSelection(allTags, selectedTag);
  $: if (state?.root && state.root !== collapsedRoot) {
    collapsedRoot = state.root;
    collapsedFolders = [];
  }
  $: filteredNotes = filterByTag(notes, selectedTag);
  $: noteTree = buildNoteTree(state?.objects ?? [], selectedTag);
  $: treeRows = flattenNoteTree(noteTree, selectedTag ? new Set<string>() : new Set(collapsedFolders));
  $: dirty = note !== null && (body !== baselineBody || JSON.stringify(tags) !== JSON.stringify(baselineTags));
  $: previewHTML = renderMarkdown(body);
  $: noteTitle = note ? fileTitle(note.relativePath) : '';
  $: managedDevices = groupManagedDevices(managedSessions);

  onMount(() => {
    appearanceMode = storedMode(localStorage.getItem('remember.appearance.mode'));
    theme = storedTheme(localStorage.getItem('remember.appearance.theme'));
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    const apply = () => applyAppearance(media.matches);
    apply();
    media.addEventListener('change', apply);

    const unsubscribe = EventsOn('remember:state', (raw: unknown) => {
      const event = parseStateEvent(raw);
      if (!event) return;
      if (event.error) {
        if (event.generation === activeGeneration) error = event.error;
        return;
      }
      if (event.state && acceptVersion(event.generation, event.revision)) {
        state = event.state;
        void syncSelectedFromDisk();
      }
    });
    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (dirty) event.preventDefault();
    };
    const shortcut = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 's') {
        event.preventDefault();
        void saveCurrent();
      }
      if (event.key === 'Escape' && contextMenu) closeContextMenu(true);
    };
    const dismissContextMenu = () => { contextMenu = null; };
    window.addEventListener('beforeunload', beforeUnload);
    window.addEventListener('keydown', shortcut);
    window.addEventListener('click', dismissContextMenu);
    window.addEventListener('blur', dismissContextMenu);
    void restoreStoredSession();
    return () => {
      unsubscribe(); media.removeEventListener('change', apply);
      window.removeEventListener('beforeunload', beforeUnload);
      window.removeEventListener('keydown', shortcut);
      window.removeEventListener('click', dismissContextMenu);
      window.removeEventListener('blur', dismissContextMenu);
    };
  });

  function openDialog(node: HTMLDialogElement) {
    node.showModal();
    return { destroy: () => { if (node.open) node.close(); } };
  }

  function applyAppearance(systemDark = window.matchMedia('(prefers-color-scheme: dark)').matches) {
    const scheme = resolvedScheme(appearanceMode, systemDark);
    document.documentElement.dataset.theme = theme;
    document.documentElement.dataset.scheme = scheme;
    document.documentElement.style.colorScheme = scheme;
  }

  function changeMode(value: string) {
    appearanceMode = storedMode(value);
    localStorage.setItem('remember.appearance.mode', appearanceMode);
    applyAppearance();
  }

  function changeTheme(value: string) {
    theme = storedTheme(value);
    localStorage.setItem('remember.appearance.theme', theme);
    applyAppearance();
  }


  async function chooseRoot() {
    await run(async () => {
      const chosen = await SelectRoot();
      if (!chosen.cancelled) selection = chosen;
    });
  }

  async function useSelectedRoot(initialize: boolean) {
    if (!selection) return;
    const selected = selection;
    await run(async () => {
      const nextState = initialize ? await InitializeRoot(selected.path) : await OpenRoot(selected.path);
      clearEditor(); applyState(nextState); selection = null;
    });
  }

  async function refresh() {
    await run(async () => { applyState(await Refresh()); await syncSelectedFromDisk(); });
  }


  async function restoreStoredSession() {
    await run(async () => { session = await RestoreSession(); });
  }
  function openLogin() {
    setAuthMode('login');
    loginOpen = true;
  }

  function closeLogin() {
    loginPassword = '';
    loginPasswordConfirm = '';
    verificationToken = '';
    loginOpen = false;
  }

  function setAuthMode(mode: AuthMode) {
    authMode = mode;
    loginPassword = '';
    loginPasswordConfirm = '';
    verificationToken = '';
  }

  async function loginCurrent() {
    if (!loginServer.trim() || !loginEmail.trim() || !loginPassword || !loginDevice.trim()) return;
    await run(async () => {
      session = await Login({
        serverUrl: loginServer.trim(), email: loginEmail.trim(),
        password: loginPassword, deviceName: loginDevice.trim(),
      });
      loginPassword = '';
      loginOpen = false;
      notice = 'Serververbindung hergestellt.';
    });
  }

  async function registerCurrent() {
    if (!loginServer.trim() || !loginEmail.trim() || loginPassword.length < 15 || loginPassword !== loginPasswordConfirm) return;
    await run(async () => {
      await RegisterAccount({ serverUrl: loginServer.trim(), email: loginEmail.trim(), password: loginPassword });
      loginPassword = '';
      loginPasswordConfirm = '';
      authMode = 'verify';
      notice = 'Registrierung angenommen. Der Verifizierungscode wurde per E-Mail versendet.';
    });
  }

  async function verifyEmailCurrent() {
    if (!loginServer.trim() || !verificationToken.trim()) return;
    await run(async () => {
      await VerifyEmail({ serverUrl: loginServer.trim(), token: verificationToken.trim() });
      verificationToken = '';
      authMode = 'login';
      notice = 'E-Mail bestätigt. Du kannst dich jetzt anmelden.';
    });
  }

  async function logoutCurrent() {
    await run(async () => {
      await Logout();
      session = null;
      accountOpen = false;
      managedSessions = [];
      notice = 'Serversitzung beendet.';
    });
  }

  async function openAccount() {
    accountOpen = true;
    managedSessions = [];
    await run(async () => {
      managedSessions = await ListSessions();
      currentDeviceName = managedSessions.find((item) => item.current)?.deviceName ?? '';
    });
  }

  async function renameCurrentDevice() {
    if (!currentDeviceName.trim()) return;
    const name = currentDeviceName.trim();
    await run(async () => {
      await RenameCurrentDevice(name);
      managedSessions = managedSessions.map((item) => item.current ? { ...item, deviceName: name } : item);
      notice = 'Gerätename aktualisiert.';
    });
  }

  async function revokeManagedSession(item: main.ManagedSessionView) {
    if (item.current || item.status !== 'active' || !window.confirm(`Sitzung auf „${item.deviceName}“ widerrufen?`)) return;
    await run(async () => {
      await RevokeSession(item.sessionId);
      managedSessions = managedSessions.filter((candidate) => candidate.sessionId !== item.sessionId);
      notice = 'Sitzung widerrufen.';
    });
  }

  async function revokeManagedDevice(device: ManagedDevice) {
    if (device.current || !window.confirm(`Gerät „${device.deviceName}“ und alle seine Sitzungen widerrufen?`)) return;
    await run(async () => {
      await RevokeDevice(device.deviceId);
      managedSessions = managedSessions.filter((item) => item.deviceId !== device.deviceId);
      notice = 'Gerät widerrufen.';
    });
  }

  function groupManagedDevices(items: main.ManagedSessionView[]): ManagedDevice[] {
    const devices = new Map<string, ManagedDevice>();
    for (const item of items) {
      const device = devices.get(item.deviceId);
      if (device) {
        device.current ||= item.current;
        if (item.current) device.deviceName = item.deviceName;
        device.sessions.push(item);
      } else {
        devices.set(item.deviceId, { deviceId: item.deviceId, deviceName: item.deviceName, current: item.current, sessions: [item] });
      }
    }
    return [...devices.values()].sort((left, right) => Number(right.current) - Number(left.current));
  }

  function formatSessionDate(raw: string): string {
    const value = new Date(raw);
    return Number.isNaN(value.getTime()) ? raw : new Intl.DateTimeFormat('de-DE', { dateStyle: 'medium', timeStyle: 'short' }).format(value);
  }

  async function syncNow() {
    if (!state || !session) return;
    await run(async () => {
      applyState(await SyncNow());
      await syncSelectedFromDisk();
      notice = 'Synchronisierung abgeschlossen.';
    });
  }

  async function changeRoot() {
    if (dirty && !window.confirm('Ungespeicherte Änderungen verwerfen und den Ordner wechseln?')) return;
    await run(async () => {
      activeGeneration = await CloseRoot(); activeRevision = 0; state = null; selection = null; clearEditor();
    });
  }

  function folderCollapsed(relativePath: string): boolean {
    return !selectedTag && collapsedFolders.includes(relativePath);
  }

  function toggleFolder(relativePath: string) {
    if (selectedTag) return;
    collapsedFolders = collapsedFolders.includes(relativePath)
      ? collapsedFolders.filter((path) => path !== relativePath)
      : [...collapsedFolders, relativePath];
  }

  async function selectNote(relativePath: string) {
    if (busy || note?.relativePath === relativePath) return;
    if (dirty && !window.confirm('Ungespeicherte Änderungen verwerfen?')) return;
    await run(async () => setLoadedNote(await ReadNote(relativePath)));
  }

  async function saveCurrent() {
    if (!note || !dirty || stale) return;
    const current = note;
    const submittedBody = body;
    const submittedTags = [...tags];
    await run(async () => {
      const mutation = await SaveNote({
        relativePath: current.relativePath, expectedRevision: current.revision,
        body: submittedBody, tags: submittedTags,
      });
      await applyMutation(mutation, { body: submittedBody, tags: submittedTags, relativePath: current.relativePath });
      notice = 'Notiz gespeichert.';
    }, true);
  }

  function openCreateNote(folder = '') {
    if (dirty) return;
    contextMenu = null;
    createFolder = folder;
    createName = '';
    createOpen = true;
  }

  function openCreateFolder(parent = '') {
    contextMenu = null;
    folderParent = parent;
    folderName = '';
    folderOpen = true;
  }

  function openFolderContextMenu(event: MouseEvent, folder: string) {
    event.preventDefault();
    event.stopPropagation();
    contextMenuTrigger = event.currentTarget as HTMLElement;
    showContextMenu(event.clientX, event.clientY, folder);
  }

  function openFolderKeyboardMenu(event: KeyboardEvent, folder: string) {
    if (!(event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10'))) return;
    event.preventDefault();
    contextMenuTrigger = event.currentTarget as HTMLElement;
    const rect = contextMenuTrigger.getBoundingClientRect();
    showContextMenu(rect.left + 24, rect.top + 24, folder);
  }

  async function showContextMenu(x: number, y: number, folder: string) {
    contextMenu = {
      x: Math.max(8, Math.min(x, window.innerWidth - 210)),
      y: Math.max(8, Math.min(y, window.innerHeight - 110)),
      folder,
    };
    await tick();
    contextMenuElement?.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
  }

  function closeContextMenu(restoreFocus = false) {
    contextMenu = null;
    if (restoreFocus) contextMenuTrigger?.focus();
  }

  function onContextMenuKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeContextMenu(true);
      return;
    }
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const buttons = [...(contextMenuElement?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)') ?? [])];
    if (!buttons.length) return;
    event.preventDefault();
    const current = buttons.indexOf(document.activeElement as HTMLButtonElement);
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 :
      event.key === 'ArrowDown' ? (current + 1) % buttons.length : (current - 1 + buttons.length) % buttons.length;
    buttons[next].focus();
  }

  async function createCurrent() {
    await run(async () => {
      const mutation = await CreateNote(createFolder, createName);
      await applyMutation(mutation); createOpen = false; createName = ''; notice = 'Notiz erstellt.';
    });
  }

  async function createFolderCurrent() {
    if (!folderName.trim()) return;
    const parent = folderParent;
    await run(async () => {
      const nextState = await CreateFolder(parent, folderName);
      applyState(nextState);
      collapsedFolders = collapsedFolders.filter((path) => path !== parent);
      folderOpen = false;
      folderName = '';
      notice = 'Ordner erstellt.';
    });
  }

  function openMove() {
    if (!note || dirty) return;
    const slash = note.relativePath.lastIndexOf('/');
    moveFolder = slash >= 0 ? note.relativePath.slice(0, slash) : '';
    moveName = fileTitle(note.relativePath);
    moveOpen = true;
  }

  async function moveCurrent() {
    if (!note || dirty || !moveName.trim()) return;
    const current = note;
    await run(async () => {
      const mutation = await MoveNote({
        relativePath: current.relativePath, expectedRevision: current.revision,
        folder: moveFolder, name: moveName,
      });
      await applyMutation(mutation); moveOpen = false; notice = 'Notiz verschoben.';
    }, true);
  }

  async function deleteCurrent() {
    if (!note) return;
    const current = note;
    await run(async () => {
      const nextState = await DeleteNote({ relativePath: current.relativePath, expectedRevision: current.revision });
      applyState(nextState); clearEditor(); deleteOpen = false; notice = 'Notiz in den lokalen Papierkorb verschoben.';
    }, true);
  }

  async function addTag() {
    const value = tagInput.trim().normalize('NFC');
    if (!value) return;
    try {
      tags = await NormalizeTags([...tags, value]);
      tagInput = '';
    } catch (caught) {
      error = caught instanceof Error ? caught.message : String(caught);
    }
  }

  function removeTag(index: number) { tags = tags.filter((_, itemIndex) => itemIndex !== index); }
  function onTagKeydown(event: KeyboardEvent) {
    if (event.key === 'Enter' || event.key === ',') { event.preventDefault(); void addTag(); }
  }

  async function applyMutation(
    mutation: main.NoteMutation,
    submitted?: { body: string; tags: string[]; relativePath: string },
  ) {
    const accepted = applyState(mutation.state);
    if (!submitted) {
      if (accepted) setLoadedNote(mutation.note);
      else { applyState(await Refresh()); await syncSelectedFromDisk(); }
      return;
    }

    const sameNote = note?.relativePath === submitted.relativePath;
    const changedAfterSubmit = hasPostSubmitChanges(body, tags, submitted.body, submitted.tags);
    const action = saveResponseAction(accepted, sameNote, changedAfterSubmit);
    if (action === 'load') {
      setLoadedNote(mutation.note);
      return;
    }
    if (action === 'preserve' || action === 'preserve-and-sync') {
      installSavedBaseline(mutation.note);
    }
    if (action === 'preserve-and-sync' || action === 'resync') {
      applyState(await Refresh());
      await syncSelectedFromDisk();
    }
  }

  function installSavedBaseline(saved: main.NoteView) {
    note = saved;
    baselineBody = saved.body;
    baselineTags = [...(saved.tags ?? [])];
    stale = '';
    syncSequence++;
  }

  function setLoadedNote(next: main.NoteView) {
    note = next; body = next.body; tags = [...(next.tags ?? [])]; baselineBody = next.body;
    baselineTags = [...(next.tags ?? [])]; stale = ''; syncSequence++;
  }

  function clearEditor() {
    note = null; body = ''; tags = []; baselineBody = ''; baselineTags = []; stale = ''; syncSequence++;
  }

  async function syncSelectedFromDisk() {
    const current = note;
    if (!current || !state) return;
    if (!state.objects.some((object) => object.type === 'note' && object.relativePath === current.relativePath)) {
      stale = 'deleted'; return;
    }
    const sequence = ++syncSequence;
    try {
      const fresh = await ReadNote(current.relativePath);
      if (sequence !== syncSequence || note?.relativePath !== current.relativePath || fresh.revision === current.revision) return;
      if (dirty) stale = 'changed'; else setLoadedNote(fresh);
    } catch {
      if (sequence === syncSequence) stale = 'deleted';
    }
  }

  async function run(action: () => Promise<void>, markConflict = false) {
    busy = true; error = ''; notice = '';
    try { await action(); }
    catch (caught) {
      const message = caught instanceof Error ? caught.message : String(caught);
      error = message;
      if (markConflict && /concurrent|changed|not found|destination/i.test(message)) stale = /not found/i.test(message) ? 'deleted' : 'changed';
    } finally { busy = false; }
  }

  function parseStateEvent(value: unknown): StateEvent | null {
    if (typeof value !== 'object' || value === null) return null;
    const candidate = value as Record<string, unknown>;
    if (typeof candidate.generation !== 'number' || typeof candidate.revision !== 'number') return null;
    if (candidate.error !== undefined && typeof candidate.error !== 'string') return null;
    if (candidate.state !== undefined && (typeof candidate.state !== 'object' || candidate.state === null)) return null;
    return candidate as StateEvent;
  }

  function acceptVersion(generation: number, revision: number): boolean {
    if (!acceptsVersion({ generation: activeGeneration, revision: activeRevision }, { generation, revision })) return false;
    activeGeneration = generation; activeRevision = revision; return true;
  }
  function applyState(nextState: main.ClientState): boolean {
    if (!acceptVersion(nextState.generation, nextState.revision)) return false;
    state = nextState;
    return true;
  }
  function fileTitle(relativePath: string): string {
    const parts = relativePath.split('/');
    const name = parts[parts.length - 1] ?? relativePath;
    return name.toLowerCase().endsWith('.md') ? name.slice(0, -3) : name;
  }
  function issueTitle(code: string): string {
    const titles: Record<string, string> = {
      ambiguous_folder_identity: 'Ordneridentität ist unklar', duplicate_note_id: 'Doppelte Notiz-ID',
      invalid_frontmatter: 'Frontmatter ist ungültig', invalid_name: 'Name ist nicht plattformkompatibel',
      name_collision: 'Namen kollidieren', unreadable: 'Datei kann nicht gelesen werden',
      unsupported_symlink: 'Symbolischer Link wird nicht unterstützt',
    };
    return titles[code] ?? code;
  }
</script>

<svelte:head><title>Remember</title></svelte:head>
<a class="skip-link" href="#main-content">Zum Inhalt springen</a>
<div class="app-shell">
  <header class="topbar">
    <div class="brand"><span class="brand-mark">R</span><div><p class="eyebrow">LOCAL-FIRST NOTES</p><h1>Remember</h1></div></div>
    <div class="header-actions">
      <label><span>Theme</span><select value={theme} on:change={(event) => changeTheme(event.currentTarget.value)}>{#each themes as item}<option value={item.value}>{item.label}</option>{/each}</select></label>
      <label><span>Darstellung</span><select value={appearanceMode} on:change={(event) => changeMode(event.currentTarget.value)}>{#each modes as item}<option value={item.value}>{item.label}</option>{/each}</select></label>
      {#if state}<button class="button ghost" on:click={refresh} disabled={busy}>Aktualisieren</button>{#if session}<button class="button primary" on:click={syncNow} disabled={busy}>Synchronisieren</button><button class="button ghost" on:click={openAccount} disabled={busy}>Sitzungen</button><button class="button ghost" on:click={logoutCurrent} disabled={busy}>Abmelden</button>{:else}<button class="button primary" on:click={openLogin} disabled={busy}>Verbinden</button>{/if}<button class="button ghost" on:click={changeRoot} disabled={busy}>Ordner wechseln</button>{/if}
    </div>
  </header>

  <main id="main-content" tabindex="-1">
    {#if error}<div class="alert error" role="alert"><strong>Aktion fehlgeschlagen</strong><span>{error}</span><button aria-label="Fehlermeldung schließen" on:click={() => error = ''}>×</button></div>{/if}
    {#if notice}<div class="alert success" role="status"><span>{notice}</span><button aria-label="Hinweis schließen" on:click={() => notice = ''}>×</button></div>{/if}

    {#if !state}
      <section class="welcome" aria-labelledby="welcome-title">
        <div class="welcome-mark" aria-hidden="true">R</div><p class="eyebrow">DEINE DATEIEN BLEIBEN DEINE DATEIEN</p>
        <h2 id="welcome-title">Markdown-Notizen, direkt in deinem Ordner.</h2>
        <p class="lead">Wähle einen lokalen Ordner. Remember arbeitet mit echten Markdown-Dateien und bleibt vollständig offline nutzbar.</p>
        <button class="button primary" on:click={chooseRoot} disabled={busy}>{busy ? 'Bitte warten …' : 'Notizordner auswählen'}</button>
        {#if selection}<div class="selection-card"><span class="status-dot" class:ready={selection.initialized}></span><div><strong>{selection.initialized ? 'Remember-Ordner erkannt' : 'Neuen Remember-Ordner anlegen'}</strong><code>{selection.path}</code></div><div class="selection-actions"><button class="button ghost" on:click={() => selection = null}>Abbrechen</button><button class="button primary" on:click={() => useSelectedRoot(!selection!.initialized)}>{selection.initialized ? 'Öffnen' : 'Initialisieren'}</button></div></div>{/if}
      </section>
    {:else}
      <section class="workspace" aria-label="Lokaler Notizbereich">
        <aside class="sidebar">
          <div class="sidebar-head"><div><p class="eyebrow">NOTIZEN</p><strong>{filteredNotes.length} von {notes.length}</strong></div><div class="sidebar-actions"><button class="icon-button secondary-icon" aria-label="Neuer Ordner" title="Neuer Ordner" on:click={() => openCreateFolder()}>▰+</button><button class="icon-button" aria-label="Neue Notiz" title={dirty ? 'Zuerst Änderungen speichern oder verwerfen' : 'Neue Notiz'} disabled={dirty} on:click={() => openCreateNote()}>＋</button></div></div>
          {#if allTags.length}<div class="tag-filters" aria-label="Nach Tag filtern"><button class:active={!selectedTag} on:click={() => selectedTag = ''}>Alle</button>{#each allTags as tag}<button class:active={selectedTag === tag.key} on:click={() => selectedTag = tag.key}>#{tag.name}</button>{/each}</div>{/if}
          <nav aria-label="Ordner und Notizen">
            <ul class="note-list tree-view">
              {#each treeRows as item (item.id)}
                <li>
                  {#if item.type === 'folder' && item.children.length > 0 && !selectedTag}
                    <button
                      class="tree-row folder-row"
                      style={`--tree-depth: ${item.depth - 1}`}
                      aria-expanded={!folderCollapsed(item.relativePath)}
                      aria-haspopup="menu"
                      aria-label={`${folderCollapsed(item.relativePath) ? 'Ordner öffnen' : 'Ordner schließen'}: ${item.name}`}
                      on:click={() => toggleFolder(item.relativePath)}
                      on:contextmenu={(event) => openFolderContextMenu(event, item.relativePath)}
                      on:keydown={(event) => openFolderKeyboardMenu(event, item.relativePath)}
                    >
                      <span class="tree-chevron" aria-hidden="true">{folderCollapsed(item.relativePath) ? '›' : '⌄'}</span>
                      <span class="tree-icon" aria-hidden="true">▰</span>
                      <span class="note-name">{item.name}</span>
                    </button>
                  {:else if item.type === 'folder'}
                    <button class="tree-row folder-row empty-folder" style={`--tree-depth: ${item.depth - 1}`} aria-label={`Ordner ${item.name}`} aria-haspopup="menu" on:contextmenu={(event) => openFolderContextMenu(event, item.relativePath)} on:keydown={(event) => openFolderKeyboardMenu(event, item.relativePath)}>
                      <span class="tree-spacer" aria-hidden="true"></span>
                      <span class="tree-icon" aria-hidden="true">▰</span>
                      <span class="note-name">{item.name}</span>
                    </button>
                  {:else}
                    <button
                      class="tree-row note-row"
                      class:active={note?.relativePath === item.relativePath}
                      style={`--tree-depth: ${item.depth - 1}`}
                      aria-current={note?.relativePath === item.relativePath ? 'page' : undefined}
                      on:click={() => selectNote(item.relativePath)}
                    >
                      <span class="tree-spacer" aria-hidden="true"></span>
                      <span class="tree-icon" aria-hidden="true">≡</span>
                      <span class="tree-content"><span class="note-name">{item.name}</span>{#if item.tags.length}<span class="mini-tags">{item.tags.map((tag) => `#${tag}`).join(' ')}</span>{/if}</span>
                    </button>
                  {/if}
                </li>
              {/each}
            </ul>
          </nav>
          {#if notes.length === 0}<p class="sidebar-empty">Noch keine Notizen.</p>{:else if treeRows.length === 0}<p class="sidebar-empty">Keine Notiz mit diesem Tag.</p>{/if}
          <details class="issues"><summary>Lokale Probleme <span>{state.issues.length}</span></summary>{#if state.issues.length}<ul>{#each state.issues as issue}<li><strong>{issueTitle(issue.code)}</strong><code>{issue.relativePath}</code></li>{/each}</ul>{:else}<p>Keine Probleme erkannt.</p>{/if}</details>
        </aside>

        <section class="editor-shell">
          {#if note}
            <div class="editor-toolbar">
              <div class="title-block"><p class="eyebrow">{note.relativePath}</p><h2>{noteTitle}{#if dirty}<span class="dirty-dot" title="Ungespeichert">●</span>{/if}</h2></div>
              <div class="toolbar-actions"><div class="segmented" aria-label="Ansicht">{#each ['edit','preview','split'] as mode}<button class:active={viewMode === mode} on:click={() => viewMode = mode as ViewMode}>{mode === 'edit' ? 'Bearbeiten' : mode === 'preview' ? 'Vorschau' : 'Geteilt'}</button>{/each}</div><button class="button ghost" on:click={openMove} disabled={busy || dirty}>Umbenennen</button><button class="button danger" on:click={() => deleteOpen = true} disabled={busy}>Löschen</button><button class="button primary" on:click={saveCurrent} disabled={busy || !dirty || !!stale}>Speichern</button></div>
            </div>
            {#if stale}<div class="stale-banner" role="alert"><strong>{stale === 'deleted' ? 'Die Datei wurde extern gelöscht oder verschoben.' : 'Die Datei wurde extern geändert.'}</strong><span>Dein Editorinhalt bleibt erhalten. Kopiere ihn bei Bedarf, aktualisiere danach bewusst.</span><button class="button ghost" on:click={() => { clearEditor(); void refresh(); }}>Editor schließen und aktualisieren</button></div>{/if}
            <div class="tag-editor"><span>Tags</span>{#each tags as tag, index}<span class="tag-chip">#{tag}<button aria-label={`Tag ${tag} entfernen`} on:click={() => removeTag(index)}>×</button></span>{/each}<input aria-label="Tag hinzufügen" placeholder="Tag + Enter" bind:value={tagInput} on:keydown={onTagKeydown} on:blur={() => void addTag()} /></div>
            <div class:split={viewMode === 'split'} class="editor-grid">
              {#if viewMode !== 'preview'}<label class="editor-pane"><span class="sr-only">Markdown bearbeiten</span><textarea bind:value={body} spellcheck="true" aria-label="Markdown bearbeiten"></textarea></label>{/if}
              {#if viewMode !== 'edit'}<article class="preview markdown-body" aria-label="Markdown-Vorschau">{@html previewHTML}</article>{/if}
            </div>
          {:else}
            <div class="editor-empty"><div class="welcome-mark small">R</div><h2>Wähle eine Notiz</h2><p>Oder erstelle eine neue Markdown-Notiz.</p><button class="button primary" on:click={() => openCreateNote()}>Neue Notiz</button></div>
          {/if}
        </section>
      </section>
    {/if}
  </main>
  <footer><span><span class:online-dot={session} class:offline-dot={!session}></span>{session ? 'Server verbunden' : 'Lokal aktiv'}</span>{#if state}<code title={state.root}>{state.root}</code>{/if}<span>{session ? 'Manuelle Synchronisierung' : 'Keine Serversitzung'}</span></footer>
</div>

{#if contextMenu}<div bind:this={contextMenuElement} class="context-menu" role="menu" tabindex="-1" aria-label={`Aktionen für ${contextMenu.folder}`} style={`left:${contextMenu.x}px;top:${contextMenu.y}px`} on:keydown={onContextMenuKeydown}><strong role="presentation">{contextMenu.folder}</strong><button role="menuitem" disabled={dirty} on:click={() => openCreateNote(contextMenu!.folder)}>＋ Neue Notiz hier</button><button role="menuitem" on:click={() => openCreateFolder(contextMenu!.folder)}>▰ Neuer Unterordner</button></div>{/if}
{#if createOpen}<dialog use:openDialog class="modal" aria-labelledby="create-title" on:close={() => createOpen = false}><form on:submit|preventDefault={createCurrent}><h2 id="create-title">Neue Notiz</h2><label>Name (optional)<input bind:value={createName} placeholder="Neue Notiz" /></label><p class="form-hint">Ohne Namen wird automatisch „Neue Notiz“ verwendet.</p><label>Ordner<select bind:value={createFolder}><option value="">Hauptordner</option>{#each folders as folder}<option value={folder}>{folder}</option>{/each}</select></label><div class="modal-actions"><button type="button" class="button ghost" on:click={() => createOpen = false}>Abbrechen</button><button class="button primary" disabled={busy}>Erstellen</button></div></form></dialog>{/if}
{#if folderOpen}<dialog use:openDialog class="modal" aria-labelledby="folder-title" on:close={() => folderOpen = false}><form on:submit|preventDefault={createFolderCurrent}><h2 id="folder-title">Neuer Ordner</h2><p>Wird angelegt in <code>{folderParent || 'Hauptordner'}</code>.</p><label>Name<input bind:value={folderName} placeholder="Neuer Ordner" /></label><div class="modal-actions"><button type="button" class="button ghost" on:click={() => folderOpen = false}>Abbrechen</button><button class="button primary" disabled={busy || !folderName.trim()}>Ordner erstellen</button></div></form></dialog>{/if}
{#if moveOpen}<dialog use:openDialog class="modal" aria-labelledby="move-title" on:close={() => moveOpen = false}><form on:submit|preventDefault={moveCurrent}><h2 id="move-title">Notiz umbenennen oder verschieben</h2><label>Name<input bind:value={moveName} /></label><label>Ordner<select bind:value={moveFolder}><option value="">Hauptordner</option>{#each folders as folder}<option value={folder}>{folder}</option>{/each}</select></label><div class="modal-actions"><button type="button" class="button ghost" on:click={() => moveOpen = false}>Abbrechen</button><button class="button primary" disabled={busy || !moveName.trim()}>Übernehmen</button></div></form></dialog>{/if}
{#if deleteOpen && note}<dialog use:openDialog class="modal" aria-labelledby="delete-title" on:close={() => deleteOpen = false}><h2 id="delete-title">„{noteTitle}“ löschen?</h2><p>{dirty ? 'Ungespeicherte Änderungen werden verworfen. ' : ''}Die Datei wird wiederherstellbar nach <code>.remember/trash</code> verschoben.</p><div class="modal-actions"><button class="button ghost" on:click={() => deleteOpen = false}>Abbrechen</button><button class="button danger" on:click={deleteCurrent} disabled={busy}>In Papierkorb verschieben</button></div></dialog>{/if}
{#if loginOpen}<dialog use:openDialog class="modal auth-modal" aria-labelledby="login-title" on:close={closeLogin}><div class="auth-tabs" aria-label="Kontozugang">{#each [{ mode: 'login', label: 'Anmelden' }, { mode: 'register', label: 'Registrieren' }, { mode: 'verify', label: 'Code bestätigen' }] as item}<button type="button" class:active={authMode === item.mode} aria-pressed={authMode === item.mode} on:click={() => setAuthMode(item.mode as AuthMode)}>{item.label}</button>{/each}</div><form on:submit|preventDefault={authMode === 'login' ? loginCurrent : authMode === 'register' ? registerCurrent : verifyEmailCurrent}><h2 id="login-title">{authMode === 'login' ? 'Mit Remember verbinden' : authMode === 'register' ? 'Remember-Konto erstellen' : 'E-Mail bestätigen'}</h2><p class="form-hint">{authMode === 'login' ? 'Der rotierende Sitzungsschlüssel wird ausschließlich in der sicheren Schlüsselablage des Betriebssystems gespeichert.' : authMode === 'register' ? 'Remember sendet einen 24 Stunden gültigen Verifizierungscode an deine E-Mail-Adresse.' : 'Füge den Verifizierungscode aus der Remember-E-Mail ein.'}</p><label>Server<input type="url" bind:value={loginServer} autocomplete="url" required /></label>{#if authMode !== 'verify'}<label>E-Mail<input type="email" bind:value={loginEmail} autocomplete="username" required /></label><label>Passwort<input type="password" bind:value={loginPassword} autocomplete={authMode === 'login' ? 'current-password' : 'new-password'} minlength={authMode === 'register' ? 15 : undefined} required /></label>{/if}{#if authMode === 'login'}<label>Gerätename<input bind:value={loginDevice} autocomplete="off" required /></label>{:else if authMode === 'register'}<label>Passwort bestätigen<input type="password" bind:value={loginPasswordConfirm} autocomplete="new-password" minlength="15" required /></label><p class="form-hint">Mindestens 15 Zeichen. Das Passwort wird nur für die Registrierung übertragen.</p>{:else}<label>Verifizierungscode<input class="verification-token" bind:value={verificationToken} autocomplete="one-time-code" maxlength="43" spellcheck="false" required /></label>{/if}<div class="modal-actions"><button type="button" class="button ghost" on:click={closeLogin}>Abbrechen</button><button class="button primary" disabled={busy || !loginServer.trim() || (authMode === 'login' && (!loginEmail.trim() || !loginPassword || !loginDevice.trim())) || (authMode === 'register' && (!loginEmail.trim() || loginPassword.length < 15 || loginPassword !== loginPasswordConfirm)) || (authMode === 'verify' && !verificationToken.trim())}>{authMode === 'login' ? 'Verbinden' : authMode === 'register' ? 'Konto erstellen' : 'Code bestätigen'}</button></div></form></dialog>{/if}
{#if accountOpen}<dialog use:openDialog class="modal account-modal" aria-labelledby="account-title" on:close={() => accountOpen = false}><h2 id="account-title">Geräte und Sitzungen</h2><p class="form-hint">Angemeldete Geräte und Sitzungen dieses Remember-Kontos.</p>{#if managedDevices.length}<form class="device-name-form" on:submit|preventDefault={renameCurrentDevice}><label>Dieses Gerät<input bind:value={currentDeviceName} autocomplete="off" required /></label><button class="button ghost" disabled={busy || !currentDeviceName.trim()}>Umbenennen</button></form><ul class="device-list">{#each managedDevices as device (device.deviceId)}<li class="device-card"><div class="device-card-head"><div><strong>{device.deviceName}</strong>{#if device.current}<span class="session-badge">Dieses Gerät</span>{/if}<small>{device.sessions.length} {device.sessions.length === 1 ? 'Sitzung' : 'Sitzungen'}</small></div>{#if !device.current}<button class="button danger" on:click={() => revokeManagedDevice(device)} disabled={busy}>Gerät widerrufen</button>{/if}</div><ul class="session-list">{#each device.sessions as item (item.sessionId)}<li><div><small>{item.status === 'active' ? 'Aktiv' : 'Widerrufen'} · erstellt <time datetime={item.createdAt}>{formatSessionDate(item.createdAt)}</time></small><small>Läuft ab <time datetime={item.expiresAt}>{formatSessionDate(item.expiresAt)}</time></small></div>{#if !item.current && item.status === 'active'}<button class="button ghost session-revoke" aria-label={`Sitzung auf ${device.deviceName} widerrufen`} on:click={() => revokeManagedSession(item)} disabled={busy}>Sitzung widerrufen</button>{/if}</li>{/each}</ul></li>{/each}</ul>{:else if busy}<p class="form-hint">Sitzungen werden geladen …</p>{/if}<div class="modal-actions"><button class="button ghost" on:click={() => accountOpen = false}>Schließen</button></div></dialog>{/if}
