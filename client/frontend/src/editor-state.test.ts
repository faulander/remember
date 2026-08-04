import { describe, expect, it } from 'vitest';
import { hasPostSubmitChanges, saveResponseAction } from './editor-state';

describe('save-in-flight editor state', () => {
  it('detects body edits made after save submission', () => {
    expect(hasPostSubmitChanges('newer text', ['one'], 'submitted text', ['one'])).toBe(true);
  });

  it('detects tag edits made after save submission', () => {
    expect(hasPostSubmitChanges('same', ['one', 'two'], 'same', ['one'])).toBe(true);
  });

  it('accepts an unchanged submitted buffer', () => {
    expect(hasPostSubmitChanges('same', ['one'], 'same', ['one'])).toBe(false);
  });

  it('preserves edits and resynchronizes when a newer watcher state won the ordering race', () => {
    expect(saveResponseAction(false, true, true)).toBe('preserve-and-sync');
    expect(saveResponseAction(false, true, false)).toBe('preserve-and-sync');
  });

  it('never installs a response for a note selected away from meanwhile', () => {
    expect(saveResponseAction(true, false, true)).toBe('resync');
  });
});
