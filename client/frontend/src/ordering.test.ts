import { describe, expect, it } from 'vitest';
import { acceptsVersion } from './ordering';

describe('state and mutation ordering', () => {
  it('rejects stale mutation payloads after a newer watcher state', () => {
    const active = { generation: 4, revision: 9 };
    expect(acceptsVersion(active, { generation: 4, revision: 8 })).toBe(false);
    expect(acceptsVersion(active, { generation: 3, revision: 99 })).toBe(false);
    expect(acceptsVersion(active, { generation: 4, revision: 9 })).toBe(true);
    expect(acceptsVersion(active, { generation: 5, revision: 0 })).toBe(true);
  });
});
