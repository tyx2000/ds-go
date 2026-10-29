// Read-only differential check against the fixed Harness source. Run by the
// assigning parent, not by a child agent. Never installs a plugin or starts a
// Harness/server. Framework injection is stubbed only to instantiate collect().
import {readFile} from 'node:fs/promises';
import {stripTypeScriptTypes} from 'node:module';
import {join, resolve} from 'node:path';
import {isDeepStrictEqual} from 'node:util';

async function main(){
const root=resolve(process.argv[2]??'/private/tmp/dsgo-official-687f282');
const source=async path=>stripTypeScriptTypes(await readFile(join(root,path),'utf8'),{mode:'transform'});
const load=code=>import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
const home=await load(await source('packages/util/home-paths/src/index.ts'));
let registry=await source('packages/shell/shell-env/src/index.ts');
registry=registry.replace(/^import\s+[^]*?\s+from\s+['"][^'"]+['"];?[ \t]*(?:\r?\n|$)/gm,'');
if (/^import\s/m.test(registry)) throw new Error('Unresolved framework import after TypeScript transform');
const prefix=`const Service=class {constructor(ctx){this.ctx=ctx}}; const z={object:()=>({}),string:()=>({})}; const DSH_ENV_PREFIX='DSH_',DSH_HOME_ENV='DSH_HOME'; const resolveDshHome=globalThis.__dsgoHomeResolver;\n`;
globalThis.__dsgoHomeResolver=home.resolveDshHome;
const {ShellEnvRegistry}=await load(prefix+registry);
delete globalThis.__dsgoHomeResolver;
const subprocess=await source('packages/subprocess/subprocess/src/index.ts');
const scrub=subprocess.match(/export function scrubbedParentEnv\([^]*?\n\}/)?.[0]?.replace(/^export /,'');
if (!scrub) throw new Error('Exact scrub function not found');
const scrubModule=await load(`export function snapshot(parent){ const process={env:parent}; const SENSITIVE_ENV_PATTERN=/KEY|PASSWORD|SECRET|TOKEN/i; const DSH_ENV_PREFIX='DSH_'; const proxyEnvironmentForChild=()=>({}); ${scrub}; return scrubbedParentEnv(); }`);
const terminal=await source('packages/terminal/terminal-bash/src/index.ts');
const persistent=terminal.match(/function childEnvironment\([^]*?\n\}/)?.[0];
if (!persistent) throw new Error('Exact model terminal environment not found');
const sanitize=await source('packages/terminal/terminal-bash/src/sanitize.ts');
const prompt=sanitize.match(/export const CONTROLLED_PROMPT = [^;\n]+;?/)?.[0];
if (!prompt) throw new Error('Exact model terminal prompt constant not found');
const persistentModule=await load(`${prompt}\n${persistent}\nexport {childEnvironment};`);
const host=await source('packages/api/terminal-controller/src/index.ts');
if (!/env:\s*\{\s*DSH_SESSION_ID:\s*agent\.id\s*,?\s*\}/.test(host)) throw new Error('Host terminal explicit environment changed');
const corpus=JSON.parse(await readFile(new URL('./managed-env-corpus.json',import.meta.url),'utf8'));
let failed=0;
for (const test of corpus.cases) {
 const env=scrubModule.snapshot(test.parent);
 let extra={};
 switch(test.kind){
 case 'fresh':{
   const ctx={get:()=>test.profile};
   const registry=new ShellEnvRegistry(ctx,{dshHome:test.home});
   extra=registry.collect(test.session?{agent:{session:{header:{id:test.session}}}}:{});
   break;
 }
 case 'persistent':extra=persistentModule.childEnvironment({owner:{id:test.session},sessionId:test.pty},'bash');break;
 case 'host':extra={DSH_SESSION_ID:test.session};break;
 case 'generic':break;
 default:throw new Error('Unknown fixture kind');
 }
 const facts=Object.fromEntries(Object.entries({...env,...extra}).filter(([key])=>key.toUpperCase().startsWith('DSH_')));
 if (!isDeepStrictEqual(facts,test.want)){failed++;process.stderr.write(JSON.stringify({name:test.name,actual:facts,expected:test.want})+'\n');}
}
process.stdout.write(JSON.stringify({checked:corpus.cases.length,failed,official:corpus.officialCommit})+'\n');
process.exitCode=failed?1:0;
}
await main().catch(error=>{
 const name=error?.name??'Error';
 const message=String(error?.message??error).replace(/data:[^\s)]+/g,'<generated module>');
 process.stderr.write(`${name}: ${message}\n`);
 process.exitCode=1;
});
