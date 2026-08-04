import { readdir, rm } from 'node:fs/promises';
import { resolve } from 'node:path';

const dist = resolve('dist');
for (const entry of await readdir(dist, { withFileTypes: true }).catch(() => [])) {
  if (entry.name === '.gitkeep') continue;
  await rm(resolve(dist, entry.name), { recursive: true, force: true });
}
