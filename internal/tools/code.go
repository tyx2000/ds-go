package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"dsgo/internal/core"
)

// Protocol traffic uses inherited fd 3. Program stdout/stderr remain ordinary captured output.
// Node's built-in stripper accepts erasable TypeScript, matching the official per-call language.
const nodeBootstrap = `
const fs = require('node:fs');
const readline = require('node:readline');
const { stripTypeScriptTypes } = require('node:module');
const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
const pending = new Map(); let sequence = 0, initialized = false;
const send = v => fs.writeSync(3, JSON.stringify(v) + '\n');
function clean(v) { if (v === undefined) return null; return JSON.parse(JSON.stringify(v)); }
rl.on('line', async line => {
  let msg; try { msg = JSON.parse(line); } catch { process.exit(71); }
  if (initialized) {
    const call = pending.get(msg.id); if (!call) return;
    pending.delete(msg.id); if (msg.error) call.reject(new Error(msg.error)); else call.resolve(msg.value);
    return;
  }
  initialized = true;
  try {
    if (typeof stripTypeScriptTypes !== 'function') throw new Error('run_code requires Node.js 22.13 or newer with module.stripTypeScriptTypes');
    const bindings = {};
    for (const name of msg.tools) bindings[name] = args => {
      if (pending.size >= 10) return Promise.reject(new Error('at most 10 overlapping tool calls are allowed'));
      const id = ++sequence;
      return new Promise((resolve,reject) => { pending.set(id,{resolve,reject}); send({type:'call',id,name,input:args ?? {}}); });
    };
    Object.freeze(bindings);
    const wrapped = 'async function __dsgo_program(tools) {\n' + msg.code + '\n}';
    const code = stripTypeScriptTypes(wrapped, {mode:'strip'});
    const fn = (0,eval)('(' + code + ')');
    const value = await fn(bindings);
    if (pending.size) throw new Error('program finished with unawaited tool calls; await all tools before returning');
    send({type:'result', value:clean(value)});
  } catch(e) { send({type:'failure',error:e && e.stack ? e.stack : String(e)}); }
  rl.close(); process.stdin.destroy();
});
`

func (r *Registry) runCode(ctx context.Context, tc core.ToolContext, input json.RawMessage) (core.ToolResult, error) {
	var a struct {
		Code      string `json:"code"`
		TimeoutMS int    `json:"timeout_ms"`
		Sandbox   string `json:"sandbox"`
	}
	if err := decode(input, &a); err != nil {
		return core.ToolResult{}, err
	}
	if len(a.Code) > maxFileBytes {
		return core.ToolResult{}, errors.New("program exceeds size limit")
	}
	timeout, err := deadline(a.TimeoutMS)
	if err != nil {
		return core.ToolResult{}, err
	}
	if runtime.GOOS == "windows" {
		return core.ToolResult{}, errors.New("run_code fd transport is not yet supported on Windows")
	}
	mode, err := requestedMode(ctx, tc, "run_code", input, a.Sandbox)
	if err != nil {
		return core.ToolResult{}, err
	}
	r.mu.RLock()
	runtimeMode := r.ptcMode
	r.mu.RUnlock()
	if tc.ExecutionMode != "" {
		runtimeMode = "per-call"
		if tc.ExecutionMode == "ptc-repl" {
			runtimeMode = "persistent-in-memory"
		}
	}
	standing := tc.Permission
	if standing == "" {
		standing = "workspace-write"
	}
	if runtimeMode == "persistent-in-memory" && tc.SessionID != "" && mode == standing {
		return r.runPersistentCode(ctx, tc, a.Code, timeout, mode)
	}
	node, err := r.nodeExecutable()
	if err != nil {
		return core.ToolResult{}, errors.New("run_code requires Node.js 22.13 or newer on PATH")
	}
	runctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var auditMu sync.Mutex
	var blocksMu sync.Mutex
	var nativeBlocks []core.Block
	var auditErr error
	reportAudit := func(err error) {
		if err == nil {
			return
		}
		auditMu.Lock()
		if auditErr == nil {
			auditErr = err
		}
		auditMu.Unlock()
		cancel()
	}
	sc, err := makeToolCommand(runctx, tc, mode, node, "--max-old-space-size=512", "--no-warnings", "-e", nodeBootstrap)
	if err != nil {
		return core.ToolResult{}, err
	}
	defer sc.cleanup()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return core.ToolResult{}, err
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	sc.cmd.ExtraFiles = []*os.File{controlWrite}
	stdin, err := sc.cmd.StdinPipe()
	if err != nil {
		return core.ToolResult{}, err
	}
	defer stdin.Close()
	logs := &boundedBuffer{limit: maxOutputBytes, onOverflow: cancel}
	sc.cmd.Stdout = logs
	sc.cmd.Stderr = logs
	if err := sc.cmd.Start(); err != nil {
		return core.ToolResult{}, fmt.Errorf("PTC start failed: %w", err)
	}
	controlWrite.Close()
	defer terminateProcess(sc.cmd)
	var writeMu sync.Mutex
	write := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}
	names := r.ptcBindingNames(tc.ExtraTools)
	if err := write(map[string]any{"code": a.Code, "tools": names}); err != nil {
		cancel()
		_ = sc.cmd.Wait()
		return core.ToolResult{}, err
	}
	type outcome struct {
		value   json.RawMessage
		failure string
		err     error
	}
	finished := make(chan outcome, 1)
	scanDone := make(chan struct{})
	var calls sync.WaitGroup
	slots := make(chan struct{}, 10)
	go func() {
		defer close(scanDone)
		s := bufio.NewScanner(controlRead)
		s.Buffer(make([]byte, 4096), maxFileBytes)
		for s.Scan() {
			var frame struct {
				Type  string          `json:"type"`
				ID    int             `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
				Value json.RawMessage `json:"value"`
				Error string          `json:"error"`
			}
			if err := json.Unmarshal(s.Bytes(), &frame); err != nil {
				finished <- outcome{err: fmt.Errorf("invalid PTC control frame: %w", err)}
				return
			}
			switch frame.Type {
			case "call":
				if frame.Name == "run_code" {
					_ = write(map[string]any{"id": frame.ID, "error": "nested run_code is not allowed"})
					continue
				}
				select {
				case slots <- struct{}{}:
				default:
					_ = write(map[string]any{"id": frame.ID, "error": "too many overlapping tool calls"})
					continue
				}
				calls.Add(1)
				go func() {
					defer calls.Done()
					defer func() { <-slots }()
					if tc.Emit != nil {
						if err := tc.Emit("tool/subcall", map[string]any{"id": frame.ID, "call_id": fmt.Sprintf("%s/%d", tc.CallID, frame.ID), "parent_id": tc.CallID, "name": frame.Name, "input": frame.Input, "input_raw": string(frame.Input)}); err != nil {
							reportAudit(err)
							return
						}
					}
					nested := tc
					nested.CallID = fmt.Sprintf("%s/%d", tc.CallID, frame.ID)
					res, e := r.dispatch(runctx, nested, frame.Name, frame.Input)
					if len(res.Blocks) > 0 {
						blocksMu.Lock()
						nativeBlocks = append(nativeBlocks, res.Blocks...)
						blocksMu.Unlock()
					}
					reply := map[string]any{"id": frame.ID}
					if e != nil {
						reply["error"] = e.Error()
					} else if res.IsError {
						reply["error"] = res.Content
					} else {
						var value any
						canonical := res.Content
						if len(res.Value) > 0 {
							canonical = string(res.Value)
						}
						d := json.NewDecoder(strings.NewReader(canonical))
						d.UseNumber()
						if d.Decode(&value) != nil {
							value = res.Content
						}
						reply["value"] = value
					}
					if tc.Emit != nil {
						if err := tc.Emit("tool/subresult", map[string]any{"id": frame.ID, "call_id": nested.CallID, "parent_id": tc.CallID, "name": frame.Name, "result": res, "error": errorString(e)}); err != nil {
							reportAudit(err)
							return
						}
					}
					_ = write(reply)
				}()
			case "result":
				finished <- outcome{value: frame.Value}
				return
			case "failure":
				finished <- outcome{failure: frame.Error}
				return
			default:
				finished <- outcome{err: fmt.Errorf("unknown PTC frame %q", frame.Type)}
				return
			}
		}
		finished <- outcome{err: fmt.Errorf("PTC control stream ended before settlement: %v", s.Err())}
	}()
	wait := make(chan error, 1)
	go func() { wait <- sc.cmd.Wait() }()
	var o outcome
	select {
	case o = <-finished:
	case <-runctx.Done():
		o.err = runctx.Err()
	}
	// Completion terminates remaining Node children and rejects unawaited tool activity.
	cancel()
	terminateProcess(sc.cmd)
	stdin.Close()
	controlRead.Close()
	<-scanDone
	calls.Wait()
	auditMu.Lock()
	if auditErr != nil {
		o.err = fmt.Errorf("PTC tool audit failed: %w", auditErr)
	}
	auditMu.Unlock()
	waitErr := <-wait
	text, truncated := logs.snapshot()
	if truncated {
		return core.ToolResult{}, errors.New("PTC output exceeded 1 MiB limit")
	}
	if o.err != nil {
		return core.ToolResult{}, fmt.Errorf("%w; process: %v; logs: %s", o.err, waitErr, text)
	}
	if o.failure != "" {
		res := result(map[string]any{"logs": text, "error": o.failure, "sandbox": map[string]string{"mode": mode, "backend": sc.backend}})
		res.IsError = true
		return withNativeBlocks(res, nativeBlocks), nil
	}
	if len(o.value) == 0 {
		o.value = json.RawMessage("null")
	}
	return withNativeBlocks(result(map[string]any{"value": o.value, "logs": text, "sandbox": map[string]string{"mode": mode, "backend": sc.backend, "enforcement": "filesystem"}}), nativeBlocks), nil
}

// Native media is transported alongside the canonical PTC JSON result. It is
// never base64-encoded into JavaScript or lost when the script returns a value.
func withNativeBlocks(result core.ToolResult, blocks []core.Block) core.ToolResult {
	if len(blocks) == 0 {
		return result
	}
	result.Blocks = append([]core.Block{{Type: "text", Text: result.Content}}, blocks...)
	return result
}
