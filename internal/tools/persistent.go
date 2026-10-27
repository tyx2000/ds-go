package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"dsgo/internal/core"
)

const persistentBootstrap = `
const fs = require('node:fs');
const {PassThrough,Writable} = require('node:stream');
const {AsyncLocalStorage} = require('node:async_hooks');
const {stripTypeScriptTypes} = require('node:module');
const readline = require('node:readline');
const repl = require('node:repl');
const storage = new AsyncLocalStorage();
const send = v => fs.writeSync(3,JSON.stringify(v)+'\n');
const rl = readline.createInterface({input:process.stdin,crlfDelay:Infinity});
const sink = new Writable({write(chunk,encoding,cb){cb();}});
let server,active=null,next=0;const pending=new Map();
rl.on('line', msg => {
  let m;try{m=JSON.parse(msg);}catch{process.exit(71);}
  if(m.type==='initialize'){
    if(server){process.exit(71);return;}
    if(typeof stripTypeScriptTypes!=='function'){send({type:'failure',error:'Node.js 22.13 or newer is required'});return;}
    server=repl.start({input:new PassThrough(),output:sink,terminal:false,useGlobal:true,ignoreUndefined:true,prompt:''});
    if(!server._domain || typeof server._domain.on!=='function'){send({type:'failure',error:'unsupported Node REPL domain interface'});return;}
    server._domain.on('error',e=>{
      const local=storage.getStore();
      if(active && local?.id===active.id){send({type:'failure',evaluation:active.id,error:e?.stack??String(e)});active=null;}
    });
    Object.defineProperty(globalThis,'_',{value:undefined,writable:true,configurable:true});
    Object.defineProperty(globalThis,'_error',{value:undefined,writable:true,configurable:true});
    const tools={};for(const name of m.tools)tools[name]=args=>{
      const local=storage.getStore();
      if(!active||!local||local.id!==active.id)return Promise.reject(new Error('tool call belongs to an inactive evaluation'));
      if(pending.size>=10)return Promise.reject(new Error('too many overlapping tool calls'));
      const id=++next;return new Promise((resolve,reject)=>{pending.set(id,{resolve,reject,evaluation:active.id});send({type:'call',id,evaluation:active.id,name,input:args??{}});});
    };globalThis.tools=Object.freeze(tools);send({type:'ready'});return;
  }
  if(m.type==='reply'){
    const p=pending.get(m.id);if(!p||!active||p.evaluation!==active.id)return;
    pending.delete(m.id);if(m.error)p.reject(new Error(m.error));else p.resolve(m.value);return;
  }
  if(m.type!=='evaluate'||!server||active){send({type:'failure',evaluation:m.evaluation,error:'invalid concurrent evaluation'});return;}
  active={id:m.evaluation};
  try{
    process.chdir(m.cwd);
    const code=stripTypeScriptTypes(m.code,{mode:'strip'});
    storage.run({id:m.evaluation},()=>server.eval(code+'\n',server.context,'dsgo-repl.ts',(err,value)=>{
      if(!active||active.id!==m.evaluation)return;
      try{
        if(pending.size)throw new Error('evaluation ended with unawaited tool calls');
        if(err)throw err;
        const result=value===undefined?null:JSON.parse(JSON.stringify(value));
        send({type:'result',evaluation:m.evaluation,value:result});
      }catch(e){send({type:'failure',evaluation:m.evaluation,error:e?.stack??String(e)});}
      active=null;
    }));
  }catch(e){send({type:'failure',evaluation:m.evaluation,error:e?.stack??String(e)});active=null;}
});
rl.on('close',()=>process.exit(0));
`

type nodeFrame struct {
	Type       string          `json:"type"`
	ID         int             `json:"id"`
	Evaluation string          `json:"evaluation"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`
	Value      json.RawMessage `json:"value"`
	Error      string          `json:"error"`
}
type nodeEvaluation struct {
	id       string
	ctx      context.Context
	tc       core.ToolContext
	logs     *boundedBuffer
	settled  chan nodeFrame
	calls    sync.WaitGroup
	slots    chan struct{}
	auditMu  sync.Mutex
	auditErr error
	blocksMu sync.Mutex
	blocks   []core.Block
}

func (e *nodeEvaluation) recordAudit(err error) {
	e.auditMu.Lock()
	if e.auditErr == nil {
		e.auditErr = err
	}
	e.auditMu.Unlock()
}

type nodeEnvironment struct {
	mu                             sync.Mutex
	writeMu                        sync.Mutex
	mode, cwd, workspace, nodePath string
	active                         *nodeEvaluation
	busy                           bool
	cmd                            *exec.Cmd
	stdin                          io.WriteCloser
	control                        *os.File
	cancel                         context.CancelFunc
	done, scanDone                 chan struct{}
	ready                          chan error
	exitErr                        error
	registry                       *Registry
	cleanup                        func()
}

type environmentOutput struct{ env *nodeEnvironment }

func (w environmentOutput) Write(b []byte) (int, error) {
	w.env.mu.Lock()
	defer w.env.mu.Unlock()
	if w.env.active != nil {
		return w.env.active.logs.Write(b)
	}
	return len(b), nil
}

func (e *nodeEnvironment) write(v any) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.stdin.Write(append(b, '\n'))
	return err
}
func (e *nodeEnvironment) stop() {
	e.cancel()
	terminateProcess(e.cmd)
	_ = e.stdin.Close()
	_ = e.control.Close()
	<-e.done
	<-e.scanDone
}
func (e *nodeEnvironment) alive() bool {
	select {
	case <-e.done:
		return false
	default:
		return true
	}
}

func (r *Registry) startEnvironment(cwd, workspace, mode, nodePath string, names []string) (*nodeEnvironment, error) {
	if nodePath == "" {
		nodePath = "node"
	}
	node, err := execLookPath(nodePath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	sc, err := makeCommandAt(ctx, cwd, workspace, mode, node, "--max-old-space-size=512", "--no-warnings", "-e", persistentBootstrap)
	if err != nil {
		cancel()
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		sc.cleanup()
		return nil, err
	}
	stdin, err := sc.cmd.StdinPipe()
	if err != nil {
		cancel()
		reader.Close()
		writer.Close()
		sc.cleanup()
		return nil, err
	}
	e := &nodeEnvironment{mode: mode, cwd: cwd, nodePath: nodePath, cmd: sc.cmd, stdin: stdin, control: reader, cancel: cancel, done: make(chan struct{}), scanDone: make(chan struct{}), ready: make(chan error, 1), registry: r, cleanup: sc.cleanup}
	sc.cmd.ExtraFiles = []*os.File{writer}
	sc.cmd.Stdout = environmentOutput{e}
	sc.cmd.Stderr = environmentOutput{e}
	if err := sc.cmd.Start(); err != nil {
		cancel()
		reader.Close()
		writer.Close()
		stdin.Close()
		sc.cleanup()
		return nil, err
	}
	writer.Close()
	go e.readLoop()
	go func() {
		err := sc.cmd.Wait()
		terminateProcess(sc.cmd)
		e.mu.Lock()
		e.exitErr = err
		e.mu.Unlock()
		sc.cleanup()
		close(e.done)
	}()
	if err := e.write(map[string]any{"type": "initialize", "tools": names}); err != nil {
		e.stop()
		return nil, err
	}
	select {
	case err := <-e.ready:
		if err != nil {
			e.stop()
			return nil, err
		}
		return e, nil
	case <-e.done:
		e.stop()
		return nil, errors.New("persistent Node exited during initialization")
	case <-time.After(5 * time.Second):
		e.stop()
		return nil, errors.New("persistent Node initialization timed out")
	}
}

func (e *nodeEnvironment) readLoop() {
	defer close(e.scanDone)
	s := bufio.NewScanner(e.control)
	s.Buffer(make([]byte, 4096), maxFileBytes)
	for s.Scan() {
		var f nodeFrame
		if err := json.Unmarshal(s.Bytes(), &f); err != nil {
			e.cancel()
			return
		}
		if f.Type == "ready" {
			e.ready <- nil
			continue
		}
		e.mu.Lock()
		active := e.active
		if active == nil {
			e.mu.Unlock()
			if f.Type == "failure" {
				select {
				case e.ready <- errors.New(f.Error):
				default:
				}
			}
			continue
		}
		if f.Evaluation != active.id {
			e.mu.Unlock()
			continue
		}
		switch f.Type {
		case "result", "failure":
			select {
			case active.settled <- f:
			default:
			}
			e.mu.Unlock()
		case "call":
			select {
			case active.slots <- struct{}{}:
			default:
				e.mu.Unlock()
				_ = e.write(map[string]any{"type": "reply", "id": f.ID, "error": "too many concurrent tool calls"})
				continue
			}
			active.calls.Add(1)
			e.mu.Unlock()
			go func() {
				defer active.calls.Done()
				defer func() { <-active.slots }()
				reply := map[string]any{"type": "reply", "id": f.ID}
				if f.Name == "run_code" {
					reply["error"] = "nested run_code is not allowed"
				} else {
					if active.tc.Emit != nil {
						if err := active.tc.Emit("tool/subcall", map[string]any{"id": f.ID, "call_id": fmt.Sprintf("%s/%d", active.tc.CallID, f.ID), "parent_id": active.tc.CallID, "name": f.Name, "input": f.Input, "input_raw": string(f.Input)}); err != nil {
							active.recordAudit(err)
							e.cancel()
							return
						}
					}
					nested := active.tc
					nested.CallID = fmt.Sprintf("%s/%d", active.tc.CallID, f.ID)
					res, err := e.registry.dispatch(active.ctx, nested, f.Name, f.Input)
					if len(res.Blocks) > 0 {
						active.blocksMu.Lock()
						active.blocks = append(active.blocks, res.Blocks...)
						active.blocksMu.Unlock()
					}
					if err != nil {
						reply["error"] = err.Error()
					} else if res.IsError {
						reply["error"] = res.Content
					} else {
						var v any
						canonical := res.Content
						if len(res.Value) > 0 {
							canonical = string(res.Value)
						}
						d := json.NewDecoder(strings.NewReader(canonical))
						d.UseNumber()
						if d.Decode(&v) != nil {
							v = res.Content
						}
						reply["value"] = v
					}
					if active.tc.Emit != nil {
						if emitErr := active.tc.Emit("tool/subresult", map[string]any{"id": f.ID, "call_id": nested.CallID, "parent_id": active.tc.CallID, "name": f.Name, "result": res, "error": errorString(err)}); emitErr != nil {
							active.recordAudit(emitErr)
							e.cancel()
							return
						}
					}
				}
				_ = e.write(reply)
			}()
		default:
			e.mu.Unlock()
			e.cancel()
			return
		}
	}
	if s.Err() != nil {
		e.cancel()
	}
}

func (r *Registry) runPersistentCode(ctx context.Context, tc core.ToolContext, code string, timeout time.Duration, mode string) (core.ToolResult, error) {
	cwd, err := filepath.EvalSymlinks(tc.CWD)
	if err != nil {
		return core.ToolResult{}, err
	}
	workspace, err := filepath.EvalSymlinks(workspaceRoot(tc))
	if err != nil {
		return core.ToolResult{}, err
	}
	names := []string{}
	for _, s := range r.Specs() {
		if s.Name != "run_code" {
			names = append(names, s.Name)
		}
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return core.ToolResult{}, errors.New("registry closed")
	}
	nodePath := r.nodePath
	if nodePath == "" {
		nodePath = "node"
	}
	e := r.environments[tc.SessionID]
	if e != nil && (e.mode != mode || e.cwd != cwd || e.workspace != workspace || e.nodePath != nodePath || !e.alive()) {
		delete(r.environments, tc.SessionID)
		r.mu.Unlock()
		e.stop()
		r.mu.Lock()
		e = nil
		if existing := r.environments[tc.SessionID]; existing != nil {
			e = existing
		}
	}
	if e == nil {
		e, err = r.startEnvironment(cwd, workspace, mode, nodePath, names)
		if err != nil {
			r.mu.Unlock()
			return core.ToolResult{}, err
		}
		e.workspace = workspace
		r.environments[tc.SessionID] = e
	}
	e.mu.Lock()
	if e.busy {
		e.mu.Unlock()
		r.mu.Unlock()
		return core.ToolResult{}, errors.New("an evaluation is already running for this session")
	}
	runctx, cancel := context.WithTimeout(ctx, timeout)
	active := &nodeEvaluation{id: newID(), ctx: runctx, tc: tc, logs: &boundedBuffer{limit: maxOutputBytes, onOverflow: e.cancel}, settled: make(chan nodeFrame, 1), slots: make(chan struct{}, 10)}
	e.active = active
	e.busy = true
	e.mu.Unlock()
	r.mu.Unlock()
	defer cancel()
	if err = e.write(map[string]any{"type": "evaluate", "evaluation": active.id, "code": code, "cwd": cwd}); err != nil {
		cancel()
	}
	var frame nodeFrame
	fatal := false
	if err == nil {
		select {
		case frame = <-active.settled:
		case <-runctx.Done():
			err = runctx.Err()
			fatal = true
		case <-e.done:
			e.mu.Lock()
			err = fmt.Errorf("persistent Node exited: %v", e.exitErr)
			e.mu.Unlock()
			fatal = true
		}
	}
	if err != nil {
		fatal = true
	}
	e.mu.Lock()
	if e.active == active {
		e.active = nil
	}
	e.mu.Unlock()
	cancel()
	active.calls.Wait()
	active.auditMu.Lock()
	if active.auditErr != nil {
		fatal = true
		err = fmt.Errorf("PTC tool audit failed: %w", active.auditErr)
	}
	active.auditMu.Unlock()
	logs, truncated := active.logs.snapshot()
	if truncated {
		fatal = true
		err = errors.New("PTC output exceeded 1 MiB limit")
	}
	if fatal {
		r.mu.Lock()
		if r.environments[tc.SessionID] == e {
			delete(r.environments, tc.SessionID)
		}
		r.mu.Unlock()
		e.stop()
		return core.ToolResult{}, err
	}
	e.mu.Lock()
	e.busy = false
	e.mu.Unlock()
	if len(frame.Value) == 0 {
		frame.Value = json.RawMessage("null")
	}
	res := result(map[string]any{"value": frame.Value, "logs": logs, "error": frame.Error, "sandbox": map[string]string{"mode": mode, "backend": sandboxBackend(mode), "enforcement": "filesystem"}, "execution_mode": "persistent-in-memory"})
	res.IsError = frame.Type == "failure"
	return withNativeBlocks(res, active.blocks), nil
}

func sandboxBackend(mode string) string {
	if mode == "full-access" {
		return "none"
	}
	if runtime.GOOS == "darwin" {
		return "macos-seatbelt"
	}
	return "linux-bubblewrap"
}
