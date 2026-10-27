// Port of DeepSeek Harness workflow-ptc runtime, pinned 687f282e8625c6b8d11ff58211a90524be2e4317. MIT.
'use strict';
const fs = require('node:fs');
const vm = require('node:vm');
const readline = require('node:readline');
const send = x => fs.writeSync(3, JSON.stringify(x) + '\n');
const pending = new Map(); let initialized = false;
const input = readline.createInterface({input:process.stdin,crlfDelay:Infinity});
input.on('line', line => {
  let m; try { m = JSON.parse(line); } catch { process.exit(71); }
  if (!initialized) { initialized = true; drive(m).catch(e => finish({value:null,stopReason:'error',error:render(e),agentsStarted:0})); return; }
  const p = pending.get(m.id); if (!p) return; pending.delete(m.id);
  if (m.error) p.reject(new WorkflowError(m.error, 'AGENT_START')); else p.resolve(m.value);
});
function finish(result) { send({type:'result',result}); input.close(); process.stdin.destroy(); }
class WorkflowError extends Error { constructor(message,code) {super(message);this.name='WorkflowError';this.code=code;} }
function fatal(e) { return e instanceof WorkflowError; }
function render(e) { try { return typeof e?.stack==='string' ? e.stack : String(e); } catch { return '[unrenderable thrown value]'; } }
function materialize(v,path='value',seen=new Set()) {
  if(v===null||typeof v==='string'||typeof v==='boolean')return v;
  if(typeof v==='number'&&Number.isFinite(v))return v;
  if(typeof v!=='object')throw new WorkflowError(path+': value is not plain JSON data','RESULT_UNSERIALIZABLE');
  if(seen.has(v))throw new WorkflowError(path+': circular references are not JSON data','RESULT_UNSERIALIZABLE');
  seen.add(v);try {
    if(Object.getOwnPropertySymbols(v).length)throw new WorkflowError(path+': symbol keys are not JSON data','RESULT_UNSERIALIZABLE');
    if(Array.isArray(v)) {
      const out=[];for(let i=0;i<v.length;i++){if(!(i in v))throw new WorkflowError(path+': sparse arrays are not JSON data','RESULT_UNSERIALIZABLE');out.push(materialize(v[i],path+'['+i+']',seen));}
      for(const k of Object.keys(v)){const n=Number(k);if(!Number.isInteger(n)||n<0||n>=v.length)throw new WorkflowError(path+': array properties are not JSON data','RESULT_UNSERIALIZABLE');}return out;
    }
    const p=Object.getPrototypeOf(v);if(p!==null&&Object.getPrototypeOf(p)!==null)throw new WorkflowError(path+': exotic prototypes are not JSON data','RESULT_UNSERIALIZABLE');
    const out=Object.create(null);for(const k of Object.keys(v))Object.defineProperty(out,k,{value:materialize(v[k],path+'.'+k,seen),enumerable:true});return out;
  }finally{seen.delete(v);}
}
function schemaCheck(s,root=true,path='schema') {
  if(!s||typeof s!=='object'||Array.isArray(s))throw new WorkflowError(path+' must be an object','UNSUPPORTED_SCHEMA');
  const known=new Set(['type','properties','required','additionalProperties','items','enum','const','oneOf']);
  for(const k of Object.keys(s))if(!known.has(k))throw new WorkflowError(path+'.'+k+' is outside the supported subset','UNSUPPORTED_SCHEMA');
  if(root&&s.type!=='object')throw new WorkflowError('schema root must have type object','UNSUPPORTED_SCHEMA');
  if(s.type!==undefined&&!['object','array','string','number','integer','boolean','null'].includes(s.type))throw new WorkflowError(path+'.type unsupported','UNSUPPORTED_SCHEMA');
  if(s.properties!==undefined){if(!s.properties||typeof s.properties!=='object'||Array.isArray(s.properties))throw new WorkflowError(path+'.properties must be an object','UNSUPPORTED_SCHEMA');for(const [k,x]of Object.entries(s.properties))schemaCheck(x,false,path+'.properties.'+k);}
  if(s.required!==undefined&&(!Array.isArray(s.required)||s.required.some(x=>typeof x!=='string')))throw new WorkflowError(path+'.required must be a string array','UNSUPPORTED_SCHEMA');
  if(s.additionalProperties!==undefined&&typeof s.additionalProperties!=='boolean')schemaCheck(s.additionalProperties,false,path+'.additionalProperties');
  if(s.items!==undefined)schemaCheck(s.items,false,path+'.items');
  if(s.enum!==undefined&&(!Array.isArray(s.enum)||!s.enum.length))throw new WorkflowError(path+'.enum must be non-empty','UNSUPPORTED_SCHEMA');
  if(s.oneOf!==undefined){if(!Array.isArray(s.oneOf)||!s.oneOf.length)throw new WorkflowError(path+'.oneOf must be non-empty','UNSUPPORTED_SCHEMA');for(const x of s.oneOf)schemaCheck(x,false,path+'.oneOf');}
}
async function drive(init) {
  let started=0,active=0,currentPhase; const waiters=[];
  const contain=p=>{p.catch(()=>{});return p;};
  const acquire=()=>active<init.concurrent?(active++,Promise.resolve()):new Promise(resolve=>waiters.push(()=>{active++;resolve();}));
  const release=()=>{active--;const next=waiters.shift();if(next)next();};
  const itemCap=(n,hook)=>{if(n>init.maxItems)throw new WorkflowError(hook+' exceeds item cap ('+init.maxItems+')','ITEM_CAP');};
  const agent=async(prompt,rawOpts)=>{
    if(typeof prompt!=='string'||!prompt.length)throw new WorkflowError('agent() requires a non-empty prompt string','INVALID_ARGUMENT');
    const opts=rawOpts===undefined?{}:materialize(rawOpts,'agent() options');
    if(!opts||typeof opts!=='object'||Array.isArray(opts))throw new WorkflowError('agent() options must be an object','INVALID_ARGUMENT');
    for(const k of Object.keys(opts))if(!['label','phase','provider','model','schema'].includes(k))throw new WorkflowError('agent() option "'+k+'" is not supported','UNSUPPORTED_OPTION');
    for(const k of ['label','phase','provider','model'])if(opts[k]!==undefined&&typeof opts[k]!=='string')throw new WorkflowError('agent() option "'+k+'" must be a string','INVALID_ARGUMENT');
    if(opts.schema!==undefined)schemaCheck(opts.schema);
    if(started>=init.maxTotal)throw new WorkflowError('this run reached its total agent cap ('+init.maxTotal+')','AGENT_CAP');
    const seq=++started; const first=prompt.split('\n')[0];
    const label=opts.label??(first.length<=48?first:first.slice(0,47)+'…');const phase=opts.phase??currentPhase;
    await acquire();try {
      const value=await new Promise((resolve,reject)=>{pending.set(seq,{resolve,reject});send({type:'agent',id:seq,request:{seq,prompt,label,...phase===undefined?{}:{phase},...opts.schema===undefined?{}:{schema:opts.schema},...opts.provider===undefined?{}:{provider:opts.provider},...opts.model===undefined?{}:{model:opts.model}}});});
      if(value.stopReason!=='completed')return null;
      return opts.schema!==undefined?(value.structured===undefined?null:value.structured):value.text;
    } finally {release();}
  };
  const parallel=async(thunks)=>{if(!Array.isArray(thunks))throw new WorkflowError('parallel() requires an array','INVALID_ARGUMENT');itemCap(thunks.length,'parallel()');thunks.forEach((t,i)=>{if(typeof t!=='function')throw new WorkflowError('parallel() item '+i+' is not a function','INVALID_ARGUMENT');});return Promise.all(thunks.map(async t=>{try{return await t();}catch(e){if(fatal(e))throw e;return null;}}));};
  const pipeline=async(items,...stages)=>{if(!Array.isArray(items)||!stages.length)throw new WorkflowError('pipeline() requires an items array and stage functions','INVALID_ARGUMENT');itemCap(items.length,'pipeline()');stages.forEach((s,i)=>{if(typeof s!=='function')throw new WorkflowError('pipeline() stage '+i+' is not a function','INVALID_ARGUMENT');});return Promise.all(items.map(async(item,i)=>{let v=item;try{for(const stage of stages)v=await stage(v,item,i);return v;}catch(e){if(fatal(e))throw e;return null;}}));};
  const phase=title=>{if(typeof title!=='string'||!title.length)throw new WorkflowError('phase() requires a non-empty title string','INVALID_ARGUMENT');currentPhase=title;send({type:'phase',title});};
  const log=message=>{if(typeof message!=='string')throw new WorkflowError('log() requires a message string','INVALID_ARGUMENT');send({type:'log',message});};
  const context=vm.createContext({agent:(...x)=>contain(agent(...x)),parallel:(...x)=>contain(parallel(...x)),pipeline:(...x)=>contain(pipeline(...x)),phase,log,args:init.args},{name:'workflow:'+init.meta.name});
  try {
    const compiled=new vm.Script('(async()=>{\n'+init.body+'\n})()',{filename:'workflow:'+init.meta.name,lineOffset:-1});
    const raw=await compiled.runInContext(context,{timeout:init.syncTimeout});
    const value=raw===undefined?null:materialize(raw,'workflow result');
    finish({value,stopReason:'completed',agentsStarted:started});
  }catch(e){finish({value:null,stopReason:'error',error:render(e),agentsStarted:started});}
}
