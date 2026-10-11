import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

// Include worker policy and every precached asset in the deployment version.
const root = new URL('../assets/', import.meta.url);
const source = await readFile(new URL('src/sw.js', import.meta.url), 'utf8');
const files = [...source.matchAll(/'\/assets\/([^']+)'/g)].map(match => match[1]);
const hash = createHash('sha256').update(source);
for (const name of [...new Set(files)].sort()) hash.update(name).update(await readFile(new URL(name, root)));
await writeFile(new URL('sw.js', root), source.replace('__ASSET_VERSION__', hash.digest('hex').slice(0, 16)));
