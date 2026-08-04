export type TreeObject = {
  id: string;
  type: string;
  relativePath: string;
  tags?: string[];
  tagKeys?: string[];
};

export type NoteTreeNode = {
  id: string;
  type: 'folder' | 'note';
  name: string;
  relativePath: string;
  tags: string[];
  children: NoteTreeNode[];
};

export type NoteTreeRow = NoteTreeNode & { depth: number };

export function buildNoteTree(objects: TreeObject[], selectedTagKey = ''): NoteTreeNode[] {
  const roots: NoteTreeNode[] = [];
  const folders = new Map<string, NoteTreeNode>();
  const folderIDs = new Map(
    objects.filter((object) => object.type === 'folder').map((object) => [object.relativePath, object.id]),
  );

  const ensureFolder = (relativePath: string): NoteTreeNode | null => {
    if (!relativePath) return null;
    const existing = folders.get(relativePath);
    if (existing) return existing;
    const slash = relativePath.lastIndexOf('/');
    const parentPath = slash >= 0 ? relativePath.slice(0, slash) : '';
    const node: NoteTreeNode = {
      id: folderIDs.get(relativePath) ?? `folder:${relativePath}`,
      type: 'folder',
      name: slash >= 0 ? relativePath.slice(slash + 1) : relativePath,
      relativePath,
      tags: [],
      children: [],
    };
    folders.set(relativePath, node);
    const parent = ensureFolder(parentPath);
    (parent?.children ?? roots).push(node);
    return node;
  };

  if (!selectedTagKey) {
    for (const object of objects) {
      if (object.type === 'folder') ensureFolder(object.relativePath);
    }
  }

  for (const object of objects) {
    if (object.type !== 'note' || (selectedTagKey && !(object.tagKeys ?? []).includes(selectedTagKey))) continue;
    const slash = object.relativePath.lastIndexOf('/');
    const parentPath = slash >= 0 ? object.relativePath.slice(0, slash) : '';
    const node: NoteTreeNode = {
      id: object.id,
      type: 'note',
      name: noteName(slash >= 0 ? object.relativePath.slice(slash + 1) : object.relativePath),
      relativePath: object.relativePath,
      tags: object.tags ?? [],
      children: [],
    };
    const parent = ensureFolder(parentPath);
    (parent?.children ?? roots).push(node);
  }

  sortNodes(roots);
  return roots;
}

export function flattenNoteTree(nodes: NoteTreeNode[], collapsed: ReadonlySet<string>, depth = 1): NoteTreeRow[] {
  const rows: NoteTreeRow[] = [];
  for (const node of nodes) {
    rows.push({ ...node, depth });
    if (node.type === 'folder' && !collapsed.has(node.relativePath)) {
      rows.push(...flattenNoteTree(node.children, collapsed, depth + 1));
    }
  }
  return rows;
}

function sortNodes(nodes: NoteTreeNode[]) {
  nodes.sort((left, right) => {
    if (left.type !== right.type) return left.type === 'folder' ? -1 : 1;
    return left.name.localeCompare(right.name, undefined, { numeric: true, sensitivity: 'base' });
  });
  for (const node of nodes) sortNodes(node.children);
}

function noteName(filename: string): string {
  return filename.toLowerCase().endsWith('.md') ? filename.slice(0, -3) : filename;
}
