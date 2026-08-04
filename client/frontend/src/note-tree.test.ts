import { describe, expect, it } from 'vitest';
import { buildNoteTree, flattenNoteTree } from './note-tree';

const objects = [
  { id: 'folder-a', type: 'folder', relativePath: 'Projekte' },
  { id: 'folder-b', type: 'folder', relativePath: 'Projekte/Remember' },
  { id: 'empty', type: 'folder', relativePath: 'Leer' },
  { id: 'one', type: 'note', relativePath: 'Inbox.md', tags: ['Privat'], tagKeys: ['privat'] },
  { id: 'two', type: 'note', relativePath: 'Projekte/Remember/Plan.md', tags: ['Arbeit'], tagKeys: ['arbeit'] },
];

describe('note tree', () => {
  it('builds folders, nested notes, root notes, and empty folders', () => {
    const tree = buildNoteTree(objects);
    expect(tree.map((node) => node.relativePath)).toEqual(['Leer', 'Projekte', 'Inbox.md']);
    expect(tree[1].children[0].relativePath).toBe('Projekte/Remember');
    expect(tree[1].children[0].children[0].relativePath).toBe('Projekte/Remember/Plan.md');
  });

  it('keeps only matching notes and their ancestors when filtering', () => {
    const tree = buildNoteTree(objects, 'arbeit');
    expect(tree).toHaveLength(1);
    expect(tree[0].relativePath).toBe('Projekte');
    expect(flattenNoteTree(tree, new Set()).map((row) => row.relativePath)).toEqual([
      'Projekte', 'Projekte/Remember', 'Projekte/Remember/Plan.md',
    ]);
  });

  it('hides descendants of collapsed folders', () => {
    const rows = flattenNoteTree(buildNoteTree(objects), new Set(['Projekte']));
    expect(rows.some((row) => row.relativePath === 'Projekte/Remember')).toBe(false);
    expect(rows.some((row) => row.relativePath === 'Inbox.md')).toBe(true);
  });
});
