import { describe, expect, it } from 'vitest';
import { filterByTag, tagFilters, validTagSelection } from './tags';

describe('backend canonical tag keys', () => {
  it('deduplicates and filters Unicode folds without JS lowercasing', () => {
    const notes = [
      { tags: ['Straße'], tagKeys: ['strasse'] },
      { tags: ['STRASSE'], tagKeys: ['strasse'] },
      { tags: ['Anderes'], tagKeys: ['anderes'] },
    ];
    expect(tagFilters(notes)).toEqual([
      { key: 'anderes', name: 'Anderes' },
      { key: 'strasse', name: 'Straße' },
    ]);
    expect(filterByTag(notes, 'strasse')).toHaveLength(2);
  });

  it('clears a selected tag that no longer exists after refresh or root change', () => {
    expect(validTagSelection([{ key: 'arbeit', name: 'Arbeit' }], 'arbeit')).toBe('arbeit');
    expect(validTagSelection([], 'arbeit')).toBe('');
  });
});
