// Development watch bundle: mirrors build.sh by prepending the pinned shadcn
// runtime (from runtime.sh) before the app.js + datastar.js bundle, unminified.
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import * as esbuild from 'esbuild';

const dir = fileURLToPath(new URL('.', import.meta.url));
const runtime = execFileSync('sh', ['./runtime.sh'], { cwd: dir, encoding: 'utf8', maxBuffer: 64 << 20 });

const ctx = await esbuild.context({
  stdin: { contents: '\nimport "./app.js";\nimport "./datastar.js";\n', resolveDir: `${dir}src`, sourcefile: 'dev-entry.js' },
  bundle: true,
  outfile: `${dir}../assets/app.js`,
  banner: { js: runtime },
  logLevel: 'info',
});
await ctx.watch();
