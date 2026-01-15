package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/tldw/tldw-agent/internal/config"
	"github.com/tldw/tldw-agent/internal/mcp/tools"
	"github.com/tldw/tldw-agent/internal/workspace"
)

const (
	defaultProtocolVersion = 1
	runnerName             = "tldw-agent-runner"
	runnerVersion          = "0.1.0"
)

type Runner struct {
	cfg      *config.Config
	upstream *Conn

	sessions   map[string]*Session
	sessionsMu sync.Mutex
	spawnFunc  func() (*Conn, *exec.Cmd, error)
	capsMu     sync.Mutex
	cachedCaps map[string]interface{}
}

type Session struct {
	id         string
	downstream *Conn
	process    *exec.Cmd
	workspace  *workspace.Session
	fsTools    *tools.FSTools
	terminal   *TerminalManager
	runErr     <-chan error
}

func NewRunner(cfg *config.Config) *Runner {
	runner := &Runner{
		cfg:      cfg,
		sessions: make(map[string]*Session),
	}
	runner.spawnFunc = runner.spawnDownstream
	return runner
}

func (r *Runner) SetSpawnFunc(spawn func() (*Conn, *exec.Cmd, error)) {
	r.spawnFunc = spawn
}

func (r *Runner) Run(stdin io.Reader, stdout io.Writer) error {
	r.upstream = NewConn(stdin, stdout)
	r.upstream.SetHandler(r.handleUpstreamRequest)
	r.upstream.SetNotificationHandler(r.handleUpstreamNotification)

	err := r.upstream.Run()
	r.shutdown()
	return err
}

func (r *Runner) handleUpstreamNotification(msg *RPCMessage) {
	// No upstream notifications are required for MVP.
}

func (r *Runner) handleUpstreamRequest(msg *RPCMessage) (*RPCResponse, error) {
	if msg.JSONRPC != "" && msg.JSONRPC != JSONRPCVersion {
		return NewErrorResponse(msg.ID, ErrInvalidReq, "unsupported jsonrpc version"), nil
	}

	switch msg.Method {
	case "initialize":
		return r.handleInitialize(msg)
	case "session/new":
		return r.handleSessionNew(msg)
	case "session/prompt":
		return r.handleSessionPrompt(msg)
	case "session/cancel":
		return r.handleSessionCancel(msg)
	case "_tldw/session/close":
		return r.handleSessionClose(msg)
	case "session/load":
		return NewErrorResponse(msg.ID, ErrMethodNotFound, "session/load not supported"), nil
	default:
		return NewErrorResponse(msg.ID, ErrMethodNotFound, "method not found"), nil
	}
}

func (r *Runner) handleInitialize(msg *RPCMessage) (*RPCResponse, error) {
	agentCapabilities := r.buildAgentCapabilities()
	result := map[string]interface{}{
		"protocolVersion": defaultProtocolVersion,
		"agentCapabilities": agentCapabilities,
		"agentInfo": map[string]string{
			"name":    runnerName,
			"title":   "TLDW ACP Runner",
			"version": runnerVersion,
		},
		"authMethods": []interface{}{},
	}

	return NewResultResponse(msg.ID, result), nil
}

type sessionNewParams struct {
	Cwd string `json:"cwd"`
}

func (r *Runner) handleSessionNew(msg *RPCMessage) (*RPCResponse, error) {
	if r.cfg.Agent.Command == "" {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "agent.command is required"), nil
	}

	var params sessionNewParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid session/new params"), nil
	}
	if params.Cwd == "" || !filepath.IsAbs(params.Cwd) {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "cwd must be an absolute path"), nil
	}

	ws := workspace.NewSession(r.cfg)
	if err := ws.SetRoot(params.Cwd); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, fmt.Sprintf("invalid cwd: %v", err)), nil
	}

	downstream, cmd, err := r.spawnFunc()
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}

	runErr := make(chan error, 1)
	go func() {
		runErr <- downstream.Run()
	}()

	session := &Session{
		downstream: downstream,
		process:    cmd,
		workspace:  ws,
		fsTools:    tools.NewFSTools(r.cfg, ws),
		terminal:   NewTerminalManager(r.cfg, ws),
		runErr:     runErr,
	}

	downstream.SetHandler(func(req *RPCMessage) (*RPCResponse, error) {
		return r.handleDownstreamRequest(session, req)
	})
	downstream.SetNotificationHandler(func(note *RPCMessage) {
		r.handleDownstreamNotification(session, note)
	})

	initParams := map[string]interface{}{
		"protocolVersion": defaultProtocolVersion,
		"clientCapabilities": map[string]interface{}{
			"fs": map[string]bool{
				"readTextFile":  true,
				"writeTextFile": true,
			},
			"terminal": r.cfg.Execution.Enabled,
		},
		"clientInfo": map[string]string{
			"name":    runnerName,
			"title":   "TLDW ACP Runner",
			"version": runnerVersion,
		},
	}

	initResp, err := downstream.Call(context.Background(), "initialize", initParams)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, fmt.Sprintf("downstream initialize failed: %v", err)), nil
	}
	if initResp != nil && initResp.Error != nil {
		return &RPCResponse{JSONRPC: JSONRPCVersion, ID: msg.ID, Error: initResp.Error}, nil
	}

	resp, err := downstream.CallRaw(context.Background(), "session/new", msg.Params)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, fmt.Sprintf("downstream session/new failed: %v", err)), nil
	}
	if resp.Error != nil {
		return &RPCResponse{JSONRPC: JSONRPCVersion, ID: msg.ID, Error: resp.Error}, nil
	}

	var sessionResult struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &sessionResult); err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, "invalid downstream session/new result"), nil
	}
	if sessionResult.SessionID == "" {
		return NewErrorResponse(msg.ID, ErrInternal, "missing downstream sessionId"), nil
	}

	session.id = sessionResult.SessionID
	r.sessionsMu.Lock()
	r.sessions[session.id] = session
	r.sessionsMu.Unlock()
	go r.watchSession(session.id, runErr)

	return NewResultResponse(msg.ID, json.RawMessage(resp.Result)), nil
}

type sessionPromptParams struct {
	SessionID string `json:"sessionId"`
}

func (r *Runner) handleSessionPrompt(msg *RPCMessage) (*RPCResponse, error) {
	var params sessionPromptParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid session/prompt params"), nil
	}

	session := r.getSession(params.SessionID)
	if session == nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "unknown session"), nil
	}

	resp, err := session.downstream.CallRaw(context.Background(), "session/prompt", msg.Params)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, fmt.Sprintf("downstream session/prompt failed: %v", err)), nil
	}
	if resp.Error != nil {
		return &RPCResponse{JSONRPC: JSONRPCVersion, ID: msg.ID, Error: resp.Error}, nil
	}

	return NewResultResponse(msg.ID, json.RawMessage(resp.Result)), nil
}

func (r *Runner) handleSessionCancel(msg *RPCMessage) (*RPCResponse, error) {
	var params sessionPromptParams
	if err := json.Unmarshal(msg.Params, &params); err == nil {
		if session := r.getSession(params.SessionID); session != nil {
			_ = session.downstream.NotifyRaw("session/cancel", msg.Params)
		}
	}

	if len(msg.ID) == 0 || string(msg.ID) == "null" {
		return nil, nil
	}
	return NewResultResponse(msg.ID, nil), nil
}

func (r *Runner) handleSessionClose(msg *RPCMessage) (*RPCResponse, error) {
	var params sessionPromptParams
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.SessionID == "" {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid session/close params"), nil
	}

	r.cleanupSession(params.SessionID)
	return NewResultResponse(msg.ID, nil), nil
}

func (r *Runner) handleDownstreamNotification(session *Session, msg *RPCMessage) {
	if r.upstream == nil {
		return
	}
	_ = r.upstream.SendMessage(msg)
}

func (r *Runner) handleDownstreamRequest(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	switch msg.Method {
	case "fs/read_text_file":
		return r.handleFSRead(session, msg)
	case "fs/write_text_file":
		return r.handleFSWrite(session, msg)
	case "terminal/create":
		return r.handleTerminalCreate(session, msg)
	case "terminal/output":
		return r.handleTerminalOutput(session, msg)
	case "terminal/wait_for_exit":
		return r.handleTerminalWait(session, msg)
	case "terminal/kill":
		return r.handleTerminalKill(session, msg)
	case "terminal/release":
		return r.handleTerminalRelease(session, msg)
	case "session/request_permission":
		return r.handlePermissionRequest(session, msg)
	default:
		return NewErrorResponse(msg.ID, ErrMethodNotFound, "method not found"), nil
	}
}

func (r *Runner) handlePermissionRequest(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	if r.upstream == nil {
		fallback := map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		}
		return NewResultResponse(msg.ID, fallback), nil
	}

	resp, err := r.upstream.CallRaw(context.Background(), "session/request_permission", msg.Params)
	if err != nil || resp == nil {
		fallback := map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		}
		return NewResultResponse(msg.ID, fallback), nil
	}
	if resp.Error != nil {
		return NewResultResponse(msg.ID, map[string]interface{}{
			"outcome": map[string]interface{}{"outcome": "cancelled"},
		}), nil
	}

	return NewResultResponse(msg.ID, json.RawMessage(resp.Result)), nil
}

func (r *Runner) handleFSRead(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Line      int    `json:"line"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid fs/read_text_file params"), nil
	}
	if params.Path == "" || !filepath.IsAbs(params.Path) {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "path must be absolute"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	args := map[string]interface{}{"path": params.Path}
	if params.Limit > 0 {
		startLine := params.Line
		if startLine <= 0 {
			startLine = 1
		}
		args["start_line"] = startLine
		args["end_line"] = startLine + params.Limit - 1
	} else if params.Line > 0 {
		args["start_line"] = params.Line
	}

	res, err := session.fsTools.Read(args)
	if err != nil || !res.OK {
		return NewErrorResponse(msg.ID, ErrInternal, "failed to read file"), nil
	}

	data, ok := res.Data.(map[string]interface{})
	if !ok {
		return NewErrorResponse(msg.ID, ErrInternal, "unexpected read result"), nil
	}
	content, _ := data["content"].(string)
	return NewResultResponse(msg.ID, map[string]interface{}{"content": content}), nil
}

func (r *Runner) handleFSWrite(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid fs/write_text_file params"), nil
	}
	if params.Path == "" || !filepath.IsAbs(params.Path) {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "path must be absolute"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	args := map[string]interface{}{"path": params.Path, "content": params.Content}
	res, err := session.fsTools.Write(args)
	if err != nil || !res.OK {
		return NewErrorResponse(msg.ID, ErrInternal, "failed to write file"), nil
	}

	return NewResultResponse(msg.ID, nil), nil
}

func (r *Runner) handleTerminalCreate(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID       string   `json:"sessionId"`
		Command         string   `json:"command"`
		Args            []string `json:"args"`
		Cwd             string   `json:"cwd"`
		OutputByteLimit int      `json:"outputByteLimit"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid terminal/create params"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	termID, err := session.terminal.Create(params.Command, params.Args, params.Cwd, params.OutputByteLimit)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}

	return NewResultResponse(msg.ID, map[string]string{"terminalId": termID}), nil
}

func (r *Runner) handleTerminalOutput(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID  string `json:"sessionId"`
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid terminal/output params"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	output, truncated, exitStatus, err := session.terminal.Output(params.TerminalID)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}

	result := map[string]interface{}{
		"output":    output,
		"truncated": truncated,
	}
	if exitStatus != nil {
		result["exitStatus"] = exitStatus
	}

	return NewResultResponse(msg.ID, result), nil
}

func (r *Runner) handleTerminalWait(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID  string `json:"sessionId"`
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid terminal/wait_for_exit params"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	status, err := session.terminal.WaitForExit(params.TerminalID)
	if err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}

	return NewResultResponse(msg.ID, map[string]interface{}{
		"exitCode": status.ExitCode,
		"signal":   status.Signal,
	}), nil
}

func (r *Runner) handleTerminalKill(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID  string `json:"sessionId"`
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid terminal/kill params"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	if err := session.terminal.Kill(params.TerminalID); err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}
	return NewResultResponse(msg.ID, nil), nil
}

func (r *Runner) handleTerminalRelease(session *Session, msg *RPCMessage) (*RPCResponse, error) {
	var params struct {
		SessionID  string `json:"sessionId"`
		TerminalID string `json:"terminalId"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "invalid terminal/release params"), nil
	}
	if params.SessionID != "" && session.id != params.SessionID {
		return NewErrorResponse(msg.ID, ErrInvalidParams, "sessionId mismatch"), nil
	}

	if err := session.terminal.Release(params.TerminalID); err != nil {
		return NewErrorResponse(msg.ID, ErrInternal, err.Error()), nil
	}
	return NewResultResponse(msg.ID, nil), nil
}

func (r *Runner) getSession(id string) *Session {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	return r.sessions[id]
}

func (r *Runner) spawnDownstream() (*Conn, *exec.Cmd, error) {
	cmd := exec.Command(r.cfg.Agent.Command, r.cfg.Agent.Args...)
	cmd.Env = append(os.Environ(), r.cfg.Agent.Env...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("downstream stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("downstream stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start downstream: %w", err)
	}

	return NewConn(stdout, stdin), cmd, nil
}
