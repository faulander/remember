import { describe, expect, it } from 'vitest';
import { resolvedScheme, storedMode, storedTheme } from './appearance';

describe('appearance preferences', () => {
  it('accepts only built-in modes and themes', () => {
    expect(storedMode('dark')).toBe('dark');
    expect(storedMode('custom')).toBe('system');
    expect(storedMode(null)).toBe('system');
    expect(storedTheme('nord')).toBe('nord');
    expect(storedTheme('file:///tmp/theme.css')).toBe('remember');
    expect(storedTheme(null)).toBe('remember');
  });

  it('tracks the system scheme only in system mode', () => {
    expect(resolvedScheme('system', true)).toBe('dark');
    expect(resolvedScheme('system', false)).toBe('light');
    expect(resolvedScheme('light', true)).toBe('light');
    expect(resolvedScheme('dark', false)).toBe('dark');
  });
});
