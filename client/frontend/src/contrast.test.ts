import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const css = readFileSync('src/style.css', 'utf8');
const pairs: [string, string, number][] = [
  ['text', 'surface', 4.5], ['muted', 'surface', 4.5], ['muted', 'surface-2', 4.5], ['muted', 'bg', 4.5],
  ['accent', 'surface', 4.5], ['accent-text', 'accent', 4.5],
  ['danger', 'danger-soft', 4.5], ['success', 'success-soft', 4.5],
  ['focus', 'surface', 3], ['focus', 'surface-2', 3], ['focus', 'bg', 3],
];

describe('theme contrast tokens', () => {
  it('meets text and focus contrast in every built-in theme/scheme', () => {
    const blocks: { name: string; body: string }[] = [];
    const root = css.match(/:root\s*\{([^}]*)\}/);
    expect(root).not.toBeNull();
    blocks.push({ name: 'remember/light', body: root![1] });
    for (const match of css.matchAll(/\[data-theme="([^"]+)"\]\[data-scheme="([^"]+)"\]\s*\{([^}]*)\}/g)) {
      blocks.push({ name: `${match[1]}/${match[2]}`, body: match[3] });
    }
    expect(blocks).toHaveLength(10);
    for (const block of blocks) {
      const tokens = new Map([...block.body.matchAll(/--([\w-]+):\s*(#[0-9a-fA-F]{3,6})/g)].map((match) => [match[1], match[2]]));
      for (const [foreground, background, minimum] of pairs) {
        const ratio = contrast(tokens.get(foreground)!, tokens.get(background)!);
        expect(ratio, `${block.name} ${foreground}/${background}`).toBeGreaterThanOrEqual(minimum);
      }
    }
    expect(css).toContain('outline:3px solid var(--focus)');
  });
});

function contrast(first: string, second: string): number {
  const [high, low] = [luminance(first), luminance(second)].sort((a, b) => b - a);
  return (high + 0.05) / (low + 0.05);
}

function luminance(hex: string): number {
  let raw = hex.slice(1);
  if (raw.length === 3) raw = [...raw].map((part) => part + part).join('');
  const channels = [0, 2, 4].map((offset) => parseInt(raw.slice(offset, offset + 2), 16) / 255)
    .map((value) => value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
  return 0.2126 * channels[0] + 0.7152 * channels[1] + 0.0722 * channels[2];
}
