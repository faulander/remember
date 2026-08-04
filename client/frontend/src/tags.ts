export type TaggedNote = { tags?: string[]; tagKeys?: string[] };
export type TagFilter = { name: string; key: string };

// Canonical keys come from Go's Unicode case folding implementation. JS must
// never try to reproduce those semantics with locale-sensitive lowercasing.
export function tagFilters(notes: TaggedNote[]): TagFilter[] {
  const byKey = new Map<string, string>();
  for (const note of notes) {
    (note.tags ?? []).forEach((name, index) => {
      const key = note.tagKeys?.[index];
      if (key !== undefined && !byKey.has(key)) byKey.set(key, name);
    });
  }
  return [...byKey].map(([key, name]) => ({ key, name })).sort((a, b) => a.name.localeCompare(b.name));
}

export function filterByTag<T extends TaggedNote>(notes: T[], key: string): T[] {
  return key ? notes.filter((note) => (note.tagKeys ?? []).includes(key)) : notes;
}

export function includesTagKey(tagKeys: string[], key: string): boolean {
  return tagKeys.includes(key);
}

export function validTagSelection(filters: TagFilter[], selectedKey: string): string {
  return selectedKey && filters.some((filter) => filter.key === selectedKey) ? selectedKey : '';
}
