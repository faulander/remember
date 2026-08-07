package reconcile

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

const IssueAmbiguousFolderIdentity IssueCode = "ambiguous_folder_identity"

// Options controls identity creation only where the caller can prove it safe.
type Options struct {
	// AllowInitialFolderIDs is true only for a newly initialized unmanaged root.
	AllowInitialFolderIDs bool
	// RecoveryMode forbids assigning folder IDs after loss of the local index.
	RecoveryMode bool
	// MoveCandidates are old logical paths reported by watcher rename hints.
	MoveCandidates []string
	// TrustedNewFolders were created by this process immediately before this
	// reconciliation and may receive a new identity even in recovery mode.
	TrustedNewFolders []string
	NewID             func() (uuid.UUID, error)
	// NewOperationID creates durable UUIDv7 Outbox operation identities.
	NewOperationID func() (uuid.UUID, error)
	// AppliedRemoteNotes suppresses Outbox derivation only for authenticated
	// remote note bytes whose exact observed SHA-256 matches the supplied hash.
	AppliedRemoteNotes   map[uuid.UUID][32]byte
	AppliedRemoteFolders map[uuid.UUID]bool
	AppliedRemoteDeletes map[uuid.UUID]bool
	// Trusted remote folder changes are accepted only with a verifier bound
	// to a persisted filesystem identity.
	TrustedRemoteFolders       map[string]uuid.UUID
	TrustedRemoteFolderMoves   map[string]string
	TrustedRemoteFolderDeletes map[string]uuid.UUID
	VerifyTrustedRemoteFolders func() error
}

// Report summarizes one completed reconciliation.
type Report struct {
	AssignedNoteIDs int
	Objects         int
	Issues          []Issue
}

// Run scans, safely inserts missing note IDs, rescans, and atomically replaces
// the reconstructable local index snapshot.
func Run(ctx context.Context, root string, index *localindex.Index, options Options) (Report, error) {
	generator := options.NewID
	if generator == nil {
		generator = frontmatter.NewNoteID
	}

	trustedFolderApply := len(options.TrustedRemoteFolders) != 0 || len(options.TrustedRemoteFolderMoves) != 0 || len(options.TrustedRemoteFolderDeletes) != 0
	if trustedFolderApply {
		if options.VerifyTrustedRemoteFolders == nil {
			return Report{}, errors.New("trusted remote folders require identity verifier")
		}
		if err := options.VerifyTrustedRemoteFolders(); err != nil {
			return Report{}, err
		}
	}
	inventory, err := Scan(root)
	if err != nil {
		return Report{}, err
	}
	if err := observeInventoryFolders(root, &inventory); err != nil {
		return Report{}, err
	}
	blocked := blockedNotePaths(inventory.Issues)
	assigned := 0
	for _, entry := range inventory.Entries {
		if trustedFolderApply || entry.Type != EntryNote || entry.NoteID != uuid.Nil || blocked[entry.RelativePath] {
			continue
		}
		id, err := generator()
		if err != nil {
			return Report{}, fmt.Errorf("generate note id: %w", err)
		}
		if _, err := repository.EnsureRootedNoteIdentity(root, entry.RelativePath, id); err != nil {
			return Report{}, fmt.Errorf("assign note identity to %q: %w", entry.RelativePath, err)
		}
		assigned++
	}
	if assigned > 0 {
		inventory, err = Scan(root)
		if err != nil {
			return Report{}, err
		}
		if err := observeInventoryFolders(root, &inventory); err != nil {
			return Report{}, err
		}
	}

	previous, err := index.ReadSnapshot(ctx)
	if err != nil {
		return Report{}, err
	}
	if trustedFolderApply {
		if err := options.VerifyTrustedRemoteFolders(); err != nil {
			return Report{}, err
		}
	}
	snapshot, issues, err := buildSnapshot(
		inventory, previous, options.AllowInitialFolderIDs, options.RecoveryMode,
		options.MoveCandidates, options.TrustedNewFolders, options.TrustedRemoteFolders, options.TrustedRemoteFolderMoves, options.TrustedRemoteFolderDeletes, generator,
	)
	if err != nil {
		return Report{}, err
	}
	observations := make(map[string]Entry)
	for _, entry := range inventory.Entries {
		if entry.Type == EntryFolder {
			observations[entry.RelativePath] = entry
		}
	}
	for i := range snapshot.Objects {
		object := &snapshot.Objects[i]
		if observed, ok := observations[object.RelativePath]; object.Type == localindex.ObjectFolder && object.IdentityState != localindex.IdentityPending && ok {
			object.FolderDevice, object.FolderInode = observed.FolderDevice, observed.FolderInode
		}
	}
	if trustedFolderApply {
		if err := options.VerifyTrustedRemoteFolders(); err != nil {
			return Report{}, err
		}
	}
	if err := captureSync(ctx, root, index, previous, &snapshot, options); err != nil {
		return Report{}, err
	}
	issues = make([]Issue, len(snapshot.Issues))
	for index, issue := range snapshot.Issues {
		issues[index] = Issue{Code: IssueCode(issue.Code), RelativePath: issue.RelativePath, Detail: issue.Detail}
	}
	return Report{AssignedNoteIDs: assigned, Objects: len(snapshot.Objects), Issues: issues}, nil
}

func observeInventoryFolders(root string, inventory *Inventory) error {
	for i := range inventory.Entries {
		entry := &inventory.Entries[i]
		if entry.Type != EntryFolder {
			continue
		}
		device, inode, err := repository.RootedFolderIdentity(root, entry.RelativePath)
		if err != nil {
			return fmt.Errorf("observe folder identity %q: %w", entry.RelativePath, err)
		}
		entry.FolderDevice, entry.FolderInode = device, inode
	}
	return nil
}

func buildSnapshot(
	inventory Inventory,
	previous localindex.Snapshot,
	allowInitialFolders bool,
	recoveryMode bool,
	moveCandidates []string,
	trustedNewFolders []string,
	trustedRemoteFolders map[string]uuid.UUID,
	trustedRemoteFolderMoves map[string]string,
	trustedRemoteFolderDeletes map[string]uuid.UUID,
	generator func() (uuid.UUID, error),
) (localindex.Snapshot, []Issue, error) {
	issues := append([]Issue(nil), inventory.Issues...)
	blockedNotes := blockedIndexedNotePaths(inventory.Issues)

	previousFolders := make(map[string]localindex.Object)
	previousNotes := make(map[uuid.UUID]localindex.Object)
	for _, object := range previous.Objects {
		switch object.Type {
		case localindex.ObjectFolder:
			previousFolders[object.RelativePath] = object
		case localindex.ObjectNote:
			previousNotes[object.ID] = object
		}
	}

	folderEntries := make(map[string]Entry)
	for _, entry := range inventory.Entries {
		if entry.Type == EntryFolder {
			folderEntries[entry.RelativePath] = entry
		}
	}
	folderObjects, folderIssues, err := reconcileFolders(
		folderEntries, previousFolders, inventory, previous,
		allowInitialFolders, recoveryMode, moveCandidates, trustedNewFolders, trustedRemoteFolders, trustedRemoteFolderMoves, trustedRemoteFolderDeletes, generator,
	)
	if err != nil {
		return localindex.Snapshot{}, nil, err
	}
	issues = append(issues, folderIssues...)

	objects := make([]localindex.Object, 0, len(inventory.Entries))
	folderPaths := make([]string, 0, len(folderObjects))
	for relative := range folderObjects {
		folderPaths = append(folderPaths, relative)
	}
	sort.Slice(folderPaths, func(i, j int) bool {
		return pathDepth(folderPaths[i]) < pathDepth(folderPaths[j]) ||
			(pathDepth(folderPaths[i]) == pathDepth(folderPaths[j]) && folderPaths[i] < folderPaths[j])
	})
	for _, relative := range folderPaths {
		object := folderObjects[relative]
		if parent, ok := folderObjects[parentPath(relative)]; ok {
			object.ParentID = parent.ID
		} else {
			object.ParentID = uuid.Nil
		}
		folderObjects[relative] = object
		objects = append(objects, object)
	}

	for _, entry := range inventory.Entries {
		if entry.Type != EntryNote || entry.NoteID == uuid.Nil || blockedNotes[entry.RelativePath] {
			continue
		}
		state := localindex.IdentityKnown
		if _, existed := previousNotes[entry.NoteID]; !existed {
			state = localindex.IdentityNew
		}
		object := localindex.Object{
			ID: entry.NoteID, Type: localindex.ObjectNote,
			RelativePath: entry.RelativePath, CollisionPath: collisionPath(entry.RelativePath),
			ContentHash: append([]byte(nil), entry.ContentHash[:]...), IdentityState: state,
		}
		if parent, ok := folderObjects[parentPath(entry.RelativePath)]; ok {
			object.ParentID = parent.ID
		}
		objects = append(objects, object)
	}

	sort.Slice(objects, func(i, j int) bool { return objects[i].RelativePath < objects[j].RelativePath })
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].RelativePath == issues[j].RelativePath {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].RelativePath < issues[j].RelativePath
	})
	localIssues := make([]localindex.Issue, len(issues))
	for i, issue := range issues {
		localIssues[i] = localindex.Issue{Code: string(issue.Code), RelativePath: issue.RelativePath, Detail: issue.Detail}
	}
	return localindex.Snapshot{Objects: objects, Issues: localIssues}, issues, nil
}

func reconcileFolders(
	current map[string]Entry,
	previous map[string]localindex.Object,
	inventory Inventory,
	previousSnapshot localindex.Snapshot,
	allowInitial bool,
	recoveryMode bool,
	moveCandidates []string,
	trustedNewFolders []string,
	trustedRemoteFolders map[string]uuid.UUID,
	trustedRemoteFolderMoves map[string]string,
	trustedRemoteFolderDeletes map[string]uuid.UUID,
	generator func() (uuid.UUID, error),
) (map[string]localindex.Object, []Issue, error) {
	_ = allowInitial // The caller-level initialize/open split proves index provenance.
	trustedNew := make(map[string]struct{}, len(trustedNewFolders))
	for _, relative := range trustedNewFolders {
		trustedNew[relative] = struct{}{}
	}
	currentSignatures := currentFolderSignatures(inventory)
	previousSignatures := indexedFolderSignatures(previousSnapshot)
	forcedMoves := inferWatcherMoves(previous, current, previousSignatures, currentSignatures, moveCandidates)
	for source, target := range trustedRemoteFolderMoves {
		if source == target || previous[source].ID == uuid.Nil || current[target].RelativePath == "" {
			return nil, nil, errors.New("invalid verified remote folder move")
		}
		for oldPath := range previous {
			if oldPath != source && !strings.HasPrefix(oldPath, source+"/") {
				continue
			}
			movedPath := target + strings.TrimPrefix(oldPath, source)
			if _, exists := current[movedPath]; exists {
				forcedMoves[oldPath] = movedPath
			}
		}
	}
	forcedTargets := make(map[string]struct{}, len(forcedMoves)+len(trustedRemoteFolders))
	for _, target := range forcedMoves {
		forcedTargets[target] = struct{}{}
	}
	for target, id := range trustedRemoteFolders {
		if _, exists := current[target]; !exists || id == uuid.Nil || id.Variant() != uuid.RFC4122 {
			return nil, nil, errors.New("invalid verified remote folder")
		}
		forcedTargets[target] = struct{}{}
	}

	oldEdges := make(map[string]map[string]struct{})
	currentEdges := make(map[string]map[string]struct{})
	addEdge := func(oldPath, currentPath string) {
		if oldEdges[oldPath] == nil {
			oldEdges[oldPath] = make(map[string]struct{})
		}
		if currentEdges[currentPath] == nil {
			currentEdges[currentPath] = make(map[string]struct{})
		}
		oldEdges[oldPath][currentPath] = struct{}{}
		currentEdges[currentPath][oldPath] = struct{}{}
	}

	for source, id := range trustedRemoteFolderDeletes {
		if previous[source].ID != id || id == uuid.Nil {
			return nil, nil, errors.New("invalid verified remote folder delete")
		}
	}
	for oldPath, old := range previous {
		trustedDelete := false
		for source := range trustedRemoteFolderDeletes {
			if oldPath == source || strings.HasPrefix(oldPath, source+"/") {
				trustedDelete = true
				break
			}
		}
		if trustedDelete {
			continue
		}
		if forcedTarget, forced := forcedMoves[oldPath]; forced {
			addEdge(oldPath, forcedTarget)
			continue
		}
		for currentPath, observed := range current {
			if _, forcedTarget := forcedTargets[currentPath]; forcedTarget {
				continue
			}
			observationMatches := old.FolderDevice == observed.FolderDevice && old.FolderInode == observed.FolderInode
			exactReliablePath := oldPath == currentPath && old.IdentityState != localindex.IdentityPending
			structureMatches := previousSignatures[oldPath] != "" && previousSignatures[oldPath] == currentSignatures[currentPath]
			if observationMatches && (exactReliablePath || structureMatches) {
				addEdge(oldPath, currentPath)
			}
		}
	}

	objects := make(map[string]localindex.Object)
	assignedOld := make(map[string]bool)
	assignedCurrent := make(map[string]bool)
	for oldPath, candidates := range oldEdges {
		if len(candidates) != 1 {
			continue
		}
		var currentPath string
		for candidate := range candidates {
			currentPath = candidate
		}
		if len(currentEdges[currentPath]) != 1 {
			continue
		}
		object := previous[oldPath]
		object.RelativePath = currentPath
		object.CollisionPath = collisionPath(currentPath)
		object.IdentityState = localindex.IdentityKnown
		objects[currentPath] = object
		assignedOld[oldPath] = true
		assignedCurrent[currentPath] = true
	}

	unresolvedOld := make([]string, 0)
	for oldPath := range previous {
		if !assignedOld[oldPath] {
			unresolvedOld = append(unresolvedOld, oldPath)
		}
	}
	unresolvedCurrent := make([]string, 0)
	for currentPath := range current {
		if !assignedCurrent[currentPath] {
			unresolvedCurrent = append(unresolvedCurrent, currentPath)
		}
	}
	sort.Strings(unresolvedOld)
	sort.Strings(unresolvedCurrent)

	var issues []Issue
	for _, currentPath := range unresolvedCurrent {
		if id, trusted := trustedRemoteFolders[currentPath]; trusted {
			objects[currentPath] = localindex.Object{ID: id, Type: localindex.ObjectFolder, RelativePath: currentPath, CollisionPath: collisionPath(currentPath), IdentityState: localindex.IdentityKnown}
			continue
		}
		if _, trusted := trustedNew[currentPath]; trusted {
			id, err := generator()
			if err != nil {
				return nil, nil, fmt.Errorf("generate trusted folder id: %w", err)
			}
			objects[currentPath] = localindex.Object{
				ID: id, Type: localindex.ObjectFolder, RelativePath: currentPath,
				CollisionPath: collisionPath(currentPath), IdentityState: localindex.IdentityNew,
			}
			continue
		}
		if recoveryMode {
			issues = append(issues, Issue{
				Code: IssueAmbiguousFolderIdentity, RelativePath: currentPath,
				Detail: "folder identity requires server metadata after index loss",
			})
			continue
		}
		if len(unresolvedOld) == 0 {
			id, err := generator()
			if err != nil {
				return nil, nil, fmt.Errorf("generate folder id: %w", err)
			}
			objects[currentPath] = localindex.Object{
				ID: id, Type: localindex.ObjectFolder, RelativePath: currentPath,
				CollisionPath: collisionPath(currentPath), IdentityState: localindex.IdentityNew,
			}
			continue
		}
		issues = append(issues, Issue{
			Code: IssueAmbiguousFolderIdentity, RelativePath: currentPath,
			Detail: "folder identity has multiple or insufficient continuity signals",
		})
	}

	// Preserve unresolved prior identities only while current ambiguous folders
	// exist. A plain deletion with no replacement candidate remains a deletion.
	if len(unresolvedCurrent) > 0 {
		for _, oldPath := range unresolvedOld {
			if _, deleted := trustedRemoteFolderDeletes[oldPath]; deleted {
				continue
			}
			old := previous[oldPath]
			old.IdentityState = localindex.IdentityPending
			objects[oldPath] = old
		}
	}
	return objects, issues, nil
}

func blockedNotePaths(issues []Issue) map[string]bool {
	blocked := make(map[string]bool)
	for _, issue := range issues {
		switch issue.Code {
		case IssueInvalidName, IssueNameCollision, IssueInvalidFrontmatter, IssueDuplicateNoteID, IssueUnreadable:
			blocked[issue.RelativePath] = true
		}
	}
	return blocked
}

func blockedIndexedNotePaths(issues []Issue) map[string]bool {
	blocked := make(map[string]bool)
	for _, issue := range issues {
		switch issue.Code {
		case IssueInvalidFrontmatter, IssueDuplicateNoteID, IssueUnreadable:
			blocked[issue.RelativePath] = true
		}
	}
	return blocked
}

func currentFolderSignatures(inventory Inventory) map[string]string {
	idCounts := make(map[uuid.UUID]int)
	for _, entry := range inventory.Entries {
		if entry.Type == EntryNote && entry.NoteID != uuid.Nil {
			idCounts[entry.NoteID]++
		}
	}
	folders := make([]string, 0)
	for _, entry := range inventory.Entries {
		if entry.Type == EntryFolder {
			folders = append(folders, entry.RelativePath)
		}
	}
	result := make(map[string]string, len(folders))
	for _, folder := range folders {
		prefix := folder + "/"
		var records []string
		for _, entry := range inventory.Entries {
			if !strings.HasPrefix(entry.RelativePath, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(entry.RelativePath, prefix)
			switch {
			case entry.Type == EntryFolder:
				records = append(records, "D:"+suffix)
			case entry.Type == EntryNote && entry.NoteID != uuid.Nil && idCounts[entry.NoteID] == 1:
				records = append(records, "N:"+suffix+":"+entry.NoteID.String())
			}
		}
		sort.Strings(records)
		result[folder] = strings.Join(records, "\x00")
	}
	return result
}

func indexedFolderSignatures(snapshot localindex.Snapshot) map[string]string {
	var folders []string
	for _, object := range snapshot.Objects {
		if object.Type == localindex.ObjectFolder {
			folders = append(folders, object.RelativePath)
		}
	}
	result := make(map[string]string, len(folders))
	for _, folder := range folders {
		prefix := folder + "/"
		var records []string
		for _, object := range snapshot.Objects {
			if !strings.HasPrefix(object.RelativePath, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(object.RelativePath, prefix)
			switch object.Type {
			case localindex.ObjectFolder:
				records = append(records, "D:"+suffix)
			case localindex.ObjectNote:
				records = append(records, "N:"+suffix+":"+object.ID.String())
			}
		}
		sort.Strings(records)
		result[folder] = strings.Join(records, "\x00")
	}
	return result
}

func inferWatcherMoves(
	previous map[string]localindex.Object,
	current map[string]Entry,
	previousSignatures, currentSignatures map[string]string,
	moveCandidates []string,
) map[string]string {
	newFolders := make(map[string]struct{})
	for currentPath := range current {
		if _, existed := previous[currentPath]; !existed {
			newFolders[currentPath] = struct{}{}
		}
	}
	var newRoots []string
	for candidate := range newFolders {
		ancestorIsNew := false
		for ancestor := parentPath(candidate); ancestor != ""; ancestor = parentPath(ancestor) {
			if _, exists := newFolders[ancestor]; exists {
				ancestorIsNew = true
				break
			}
		}
		if !ancestorIsNew {
			newRoots = append(newRoots, candidate)
		}
	}
	sort.Strings(newRoots)
	moveCandidates = append([]string(nil), moveCandidates...)
	sort.Slice(moveCandidates, func(i, j int) bool { return pathDepth(moveCandidates[i]) < pathDepth(moveCandidates[j]) })

	forced := make(map[string]string)
	usedTargets := make(map[string]bool)
	for _, oldRoot := range moveCandidates {
		if _, exists := previous[oldRoot]; !exists {
			continue
		}
		var candidates []string
		for _, newRoot := range newRoots {
			if usedTargets[newRoot] {
				continue
			}
			oldSignature := previousSignatures[oldRoot]
			if oldSignature == "" || oldSignature == currentSignatures[newRoot] {
				candidates = append(candidates, newRoot)
			}
		}
		if len(candidates) != 1 {
			continue
		}
		newRoot := candidates[0]
		usedTargets[newRoot] = true
		for oldPath := range previous {
			if oldPath != oldRoot && !strings.HasPrefix(oldPath, oldRoot+"/") {
				continue
			}
			suffix := strings.TrimPrefix(oldPath, oldRoot)
			target := newRoot + suffix
			if _, exists := current[target]; exists {
				forced[oldPath] = target
			}
		}
	}
	return forced
}

func collisionPath(relative string) string {
	parts := strings.Split(relative, "/")
	for i := range parts {
		parts[i] = naming.CollisionKey(parts[i])
	}
	return strings.Join(parts, "/")
}

func parentPath(relative string) string {
	parent := path.Dir(relative)
	if parent == "." {
		return ""
	}
	return parent
}

func pathDepth(relative string) int { return strings.Count(relative, "/") }

func pathFromLogical(root, relative string) string {
	return filepath.Join(root, filepath.FromSlash(relative))
}
