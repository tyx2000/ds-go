// Run by the parent/test runner, never by production dsgo. No installation,
// server process, network calls, or package lifecycle scripts are involved.
// Requires the SHA-verified SDK sources and zod@4.4.3 extracted read-only.
import {readFile} from 'node:fs/promises';
import {stripTypeScriptTypes} from 'node:module';
import {resolve, join} from 'node:path';
import {pathToFileURL} from 'node:url';

const sdk = resolve(process.argv[2] ?? '/private/tmp/dsgo-sdk-2.2.0-exact');
const zod = resolve(process.argv[3] ?? '/private/tmp/dsgo-zod-4.4.3/package');
const constantsPath = resolve(process.argv[4] ?? '/private/tmp/dsgo-sdk-2.2.0-src/core-constants.ts');
const root = join(sdk, 'core-internal/src/wire');
const constantsSource = await readFile(constantsPath, 'utf8');
const keys = ['CLIENT_CAPABILITIES_META_KEY', 'CLIENT_INFO_META_KEY', 'LOG_LEVEL_META_KEY', 'PROTOCOL_VERSION_META_KEY', 'SERVER_INFO_META_KEY'];
const constants = keys.map(key => {
  const declaration = constantsSource.match(new RegExp(`export const ${key} = '[^']+';`));
  if (!declaration) throw new Error(`Missing exact SDK constant ${key}`);
  return declaration[0];
}).join('\n');
const families = stripTypeScriptTypes(await readFile(join(root, 'resultFamilies.ts'), 'utf8'));
async function schemaModule(era) {
  const source = await readFile(join(root, `rev${era}/buildSchemas.ts`), 'utf8');
  let code = stripTypeScriptTypes(source);
  const zodImport = `import * as z from ${JSON.stringify(pathToFileURL(join(zod, 'v4/index.js')).href)};`;
  if (!code.includes("import * as z from 'zod/v4';")) throw new Error('SDK zod import changed');
  code = code.replace("import * as z from 'zod/v4';", zodImport);
  // Change only import resolution; retain the exact SDK schema definitions.
  code = code.replace(/import\s*\{[^}]+\}\s*from\s*'\.\.\/\.\.\/types\/constants';/, '');
  code = code.replace(/import\s*\{[^}]+\}\s*from\s*'\.\.\/resultFamilies';/, '');
  return import(`data:text/javascript;base64,${Buffer.from(`${constants}\n${families}\n${code}`).toString('base64')}`);
}
const legacy = (await schemaModule('2025-11-25')).buildSchemas2025();
const modern = (await schemaModule('2026-07-28')).buildSchemas2026();
const methods = {
  'tools/call': 'CallToolResultSchema',
  'tools/list': 'ListToolsResultSchema',
  'resources/list': 'ListResourcesResultSchema',
  'resources/templates/list': 'ListResourceTemplatesResultSchema',
  'resources/read': 'ReadResourceResultSchema',
  'initialize': 'InitializeResultSchema',
  'server/discover': 'DiscoverResultSchema',
};
const corpus = JSON.parse(await readFile(new URL('./wire-corpus.json', import.meta.url), 'utf8'));
let checked = 0, failed = 0;
for (const test of corpus.cases) {
  const schemas = test.era === 'modern' ? modern : legacy;
  const name = test.era === 'legacy' && test.method === 'tools/call' ? 'CallToolResultWireSchema' : methods[test.method];
  if (!schemas[name]) throw new Error(`Unknown SDK schema ${name}`);
  const result = schemas[name].safeParse(test.value);
  checked++;
  if (result.success !== test.valid) {
    failed++;
    process.stderr.write(JSON.stringify({name:test.name,era:test.era,schema:name,expected:test.valid,actual:result.success,issues:result.error?.issues})+'\n');
  }
}
process.stdout.write(JSON.stringify({checked,failed,sdk:'2.2.0',zod:'4.4.3'})+'\n');
process.exitCode = failed ? 1 : 0;
