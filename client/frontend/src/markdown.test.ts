import { describe, expect, it } from 'vitest';
import { renderMarkdown } from './markdown';

describe('renderMarkdown', () => {
  it('removes scripts and event handlers', () => {
    const html = renderMarkdown('<script>alert(1)</script><p onclick="alert(2)">safe</p>');
    expect(html).not.toMatch(/script|onclick|alert/i);
    expect(html).toContain('safe');
  });

  it('removes javascript and data links', () => {
    const html = renderMarkdown('[bad](javascript:alert(1)) <a href="data:text/html,x">data</a>');
    expect(html).not.toMatch(/javascript:|data:/i);
  });

  it('blocks SVG, data, remote, and local images', () => {
    const html = renderMarkdown([
      '<svg><script>alert(1)</script></svg>',
      '![data](data:image/svg+xml,x)',
      '![remote](https://example.com/tracker.png)',
      '![local](file:///tmp/private.png)',
    ].join('\n'));
    expect(html).not.toMatch(/<img|<svg|data:|example\.com|file:/i);
  });

  it('hardens allowed web links', () => {
    const html = renderMarkdown('[site](https://example.com)');
    expect(html).toContain('target="_blank"');
    expect(html).toContain('rel="noopener noreferrer nofollow"');
  });
});
