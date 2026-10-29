// Read the verified fixed source and execute only its pure renderer. --write
// records exact upstream output for Go's offline regression corpus.
import {readFile,writeFile} from 'node:fs/promises';
import {stripTypeScriptTypes} from 'node:module';
import {join,resolve} from 'node:path';
import {isDeepStrictEqual} from 'node:util';
async function main(){
 const root=resolve(process.argv.find(value=>value.startsWith('--source='))?.slice(9)??'/private/tmp/dsgo-official-687f282');
 const load=code=>import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`);
 const home=stripTypeScriptTypes(await readFile(join(root,'packages/util/home-paths/src/index.ts'),'utf8'),{mode:'transform'});
 const homeURL=`data:text/javascript;base64,${Buffer.from(home).toString('base64')}`;
 let renderer=stripTypeScriptTypes(await readFile(join(root,'packages/context/agent-instructions/src/render.ts'),'utf8'),{mode:'transform'});
 renderer=renderer.replace(/(['"])@deepseek-ai\/dsh-home-paths\1/g,JSON.stringify(homeURL));
 const upstream=await load(renderer);
 const fixture=JSON.parse(await readFile(new URL('./instruction-render-inputs.json',import.meta.url),'utf8'));
 const outputs=[];
 for(const test of fixture.cases){
  if(test.kind==='baseline'){
   const result=upstream.renderAgentInstructionSet(test.files,{maxBytes:test.maxBytes,replacePreviousBaseline:test.replace});
   outputs.push({...test,expected:{...result.rendered,included:result.included.map(file=>file.absolutePath)}});
  }else outputs.push({...test,expected:upstream.renderInstructionChanges(test.items,test.maxBytes)});
 }
 const path=new URL('./instruction-render-golden.json',import.meta.url);
 if(process.argv.includes('--write'))await writeFile(path,JSON.stringify({official:fixture.official,cases:outputs},null,2)+'\n');
 else{
  const stored=JSON.parse(await readFile(path,'utf8'));
  if(!isDeepStrictEqual(stored,{official:fixture.official,cases:outputs}))throw new Error('Instruction renderer golden differs from fixed source');
 }
 process.stdout.write(JSON.stringify({checked:outputs.length,failed:0,official:fixture.official})+'\n');
}
await main().catch(error=>{process.stderr.write(`${error?.name??'Error'}: ${String(error?.message??error).replace(/data:[^\s)]+/g,'<generated module>')}\n`);process.exitCode=1});
