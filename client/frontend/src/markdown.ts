import DOMPurify from 'dompurify';
import { marked } from 'marked';

const forbiddenTags = [
  'script', 'style', 'iframe', 'object', 'embed', 'form', 'input', 'button',
  'textarea', 'select', 'option', 'svg', 'math', 'img', 'picture', 'source',
  'video', 'audio', 'canvas', 'template', 'link', 'meta', 'base',
];

// renderMarkdown treats every note as hostile input. Images are deliberately
// omitted so preview never performs remote, data, or local filesystem loads.
export function renderMarkdown(markdown: string): string {
  const rendered = marked.parse(markdown, { async: false, gfm: true }) as string;
  const clean = DOMPurify.sanitize(rendered, {
    FORBID_TAGS: forbiddenTags,
    FORBID_ATTR: ['style', 'src', 'srcset', 'formaction', 'xlink:href'],
    ALLOW_DATA_ATTR: false,
  }) as string;
  const document = new DOMParser().parseFromString(clean, 'text/html');
  for (const link of document.querySelectorAll('a')) {
    const href = link.getAttribute('href')?.trim() ?? '';
    if (!safeLink(href)) {
      link.removeAttribute('href');
      link.removeAttribute('target');
    } else {
      link.setAttribute('target', '_blank');
      link.setAttribute('rel', 'noopener noreferrer nofollow');
    }
  }
  return document.body.innerHTML;
}

function safeLink(href: string): boolean {
  if (href.startsWith('#')) return true;
  try {
    const parsed = new URL(href);
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' || parsed.protocol === 'mailto:';
  } catch {
    return false;
  }
}
