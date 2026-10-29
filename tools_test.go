package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func callTool(t *testing.T, tool *tools, name string, a any) toolResult {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return tool.execute(ctx, toolCall{Identifier: "lobe-local-system", APIName: name, Arguments: string(data)})
}
func assertOK(t *testing.T, r toolResult) {
	t.Helper()
	if !r.Success {
		t.Fatalf("%+v", r)
	}
}

func TestFileToolsAndAliases(t *testing.T) {
	home := t.TempDir()
	tool := newTools(home, "/bin/sh")
	defer tool.close()
	cwd := filepath.Join(home, "workspace")
	path := filepath.Join(cwd, "sub", "a.txt")
	for _, name := range []string{"writeFile", "writeLocalFile"} {
		assertOK(t, callTool(t, tool, name, map[string]any{"cwd": cwd, "path": "sub/a.txt", "content": "alpha\nbeta\nbeta\n"}))
	}
	for _, name := range []string{"readFile", "readLocalFile"} {
		r := callTool(t, tool, name, map[string]any{"cwd": cwd, "path": "sub/a.txt", "loc": []int{1, 2}})
		assertOK(t, r)
		if !strings.Contains(r.Content, path) || r.State.(map[string]any)["content"] != "beta" {
			t.Fatal(r)
		}
	}
	r := callTool(t, tool, "editFile", map[string]any{"cwd": cwd, "file_path": "sub/a.txt", "old_string": "beta", "new_string": "gamma"})
	if r.Success || !strings.Contains(r.Error, "ambiguous") {
		t.Fatal(r)
	}
	assertOK(t, callTool(t, tool, "editLocalFile", map[string]any{"cwd": cwd, "file_path": "sub/a.txt", "old_string": "beta", "new_string": "gamma", "replace_all": true}))
	assertOK(t, callTool(t, tool, "editFile", map[string]any{"cwd": cwd, "file_path": "sub/a.txt", "old_string": "alpha", "new_string": "delta"}))
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "delta\ngamma\ngamma\n" {
		t.Fatalf("%s %v", data, err)
	}
	for _, name := range []string{"listFiles", "listLocalFiles"} {
		r := callTool(t, tool, name, map[string]any{"cwd": cwd, "path": "sub"})
		assertOK(t, r)
		if !strings.Contains(r.Content, path) {
			t.Fatal(r)
		}
	}
	for _, name := range []string{"globFiles", "globLocalFiles"} {
		r := callTool(t, tool, name, map[string]any{"cwd": home, "scope": "workspace", "pattern": "**/*.txt"})
		assertOK(t, r)
		if !strings.Contains(r.Content, path) {
			t.Fatal(r)
		}
	}
	r = callTool(t, tool, "grepContent", map[string]any{"cwd": cwd, "pattern": "gam.*"})
	assertOK(t, r)
	if !strings.Contains(r.Content, path+":2:gamma") || r.State.(map[string]any)["totalMatches"] != 2 {
		t.Fatal(r)
	}
	r = callTool(t, tool, "readFile", map[string]any{"cwd": cwd, "path": "missing"})
	if r.Success || !strings.Contains(r.Content, filepath.Join(cwd, "missing")) {
		t.Fatal(r)
	}
}

func TestCommandsForegroundBackgroundPollKill(t *testing.T) {
	home := t.TempDir()
	tool := newTools(home, "/bin/sh")
	defer tool.close()
	r := callTool(t, tool, "runCommand", map[string]any{"command": "printf 'hello'; printf 'problem' >&2; exit 7", "cwd": home})
	state := r.State.(map[string]any)
	if r.Success || state["exitCode"] != 7 || state["stdout"] != "hello" || state["stderr"] != "problem" || state["isBackground"] != false || !strings.Contains(r.Content, "exit_code: 7") {
		t.Fatal(r)
	}
	r = callTool(t, tool, "runCommand", map[string]any{"command": "pwd", "cwd": home})
	assertOK(t, r)
	if !strings.Contains(r.Content, home) {
		t.Fatal(r)
	}
	start := time.Now()
	r = callTool(t, tool, "runCommand", map[string]any{"command": "printf first; sleep 0.1; printf second; sleep 10", "run_in_background": true})
	assertOK(t, r)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("background did not return immediately")
	}
	id := r.State.(map[string]any)["commandId"].(string)
	if !strings.Contains(r.Content, "shell_id: "+id) {
		t.Fatal(r)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r = callTool(t, tool, "getCommandOutput", map[string]any{"shell_id": id})
		assertOK(t, r)
		if r.State.(map[string]any)["running"] != true {
			t.Fatal(r)
		}
		if strings.Contains(r.Content, "firstsecond") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("output not retained", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertOK(t, callTool(t, tool, "killCommand", map[string]any{"shell_id": id}))
	r = callTool(t, tool, "getCommandOutput", map[string]any{"shell_id": id})
	assertOK(t, r)
	if r.State.(map[string]any)["running"] != false || !strings.Contains(r.Content, "firstsecond") || !strings.Contains(r.Content, "exit_code: -1") {
		t.Fatal(r)
	}
	assertOK(t, callTool(t, tool, "killCommand", map[string]any{"shell_id": id}))
	if callTool(t, tool, "killCommand", map[string]any{"shell_id": "unknown"}).Success {
		t.Fatal("unknown process accepted")
	}
}

func TestCommandTimeoutKillsChildren(t *testing.T) {
	for _, bg := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreground", true: "background"}[bg], func(t *testing.T) {
			home := t.TempDir()
			tool := newTools(home, "/bin/sh")
			defer tool.close()
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			data, _ := json.Marshal(map[string]any{"command": "(sleep 0.2; echo orphan > escaped) & wait", "cwd": home, "run_in_background": bg})
			r := tool.execute(ctx, toolCall{Identifier: "lobe-local-system", APIName: "runCommand", Arguments: string(data)})
			if bg {
				assertOK(t, r)
			} else if r.Success {
				t.Fatal("timeout succeeded")
			}
			time.Sleep(300 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(home, "escaped")); !os.IsNotExist(err) {
				t.Fatal("child survived timeout", err)
			}
			id := r.State.(map[string]any)["commandId"].(string)
			r = callTool(t, tool, "getCommandOutput", map[string]any{"shell_id": id})
			if r.State.(map[string]any)["running"] != false || !strings.Contains(r.Content, "timed out") {
				t.Fatal(r)
			}
		})
	}
}

func TestRefusalsAndInvalidArguments(t *testing.T) {
	tool := newTools(t.TempDir(), "/bin/sh")
	defer tool.close()
	for _, name := range []string{"browser", "computerUse", "openTerminal", "initWorkspace", "metrics", "login", "refreshToken", "unknown"} {
		r := callTool(t, tool, name, map[string]any{})
		if r.Success || r.Content != "" || !strings.Contains(r.Error, "unsupported:") {
			t.Fatal(r)
		}
	}
	for _, option := range []string{"sandbox", "useSandbox", "pty"} {
		r := callTool(t, tool, "runCommand", map[string]any{"command": "echo bad", option: true})
		if r.Success || !strings.Contains(r.Error, "unsupported:") {
			t.Fatal(r)
		}
	}
	for _, raw := range []string{"{", "null", "[]", `{"path":4}`} {
		r := tool.execute(context.Background(), toolCall{Identifier: "lobe-local-system", APIName: "readFile", Arguments: raw})
		if r.Success {
			t.Fatal(raw)
		}
	}
	for _, kind := range []string{"mcp", "browser"} {
		r := tool.execute(context.Background(), toolCall{Identifier: "lobe-local-system", Type: kind, APIName: "runCommand", Arguments: `{"command":"echo bad"}`})
		if r.Success || r.Content != "" {
			t.Fatal(r)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := tool.execute(ctx, toolCall{Identifier: "lobe-local-system", APIName: "writeFile", Arguments: `{"path":"bad","content":"bad"}`})
	if r.Success {
		t.Fatal(r)
	}
}

func TestSpecialFilesDoNotBlock(t *testing.T) {
	home := t.TempDir()
	tool := newTools(home, "/bin/sh")
	defer tool.close()
	fifo := filepath.Join(home, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan toolResult, 1)
	go func() { done <- callTool(t, tool, "readFile", map[string]any{"path": fifo}) }()
	select {
	case r := <-done:
		if r.Success {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO read blocked")
	}
}

func TestCommandEnvironmentAndArgumentTimeout(t *testing.T) {
	tool := newTools(t.TempDir(), "/bin/sh")
	defer tool.close()
	r := callTool(t, tool, "runCommand", map[string]any{"command": "printf '%s' \"$LOBE_DEVICE_TEST\"", "env": map[string]string{"LOBE_DEVICE_TEST": "environment-value"}})
	assertOK(t, r)
	if r.State.(map[string]any)["stdout"] != "environment-value" {
		t.Fatal(r)
	}
	r = callTool(t, tool, "runCommand", map[string]any{"command": "sleep 10", "timeout": 30})
	if r.Success || !strings.Contains(r.Content, "timed out") {
		t.Fatal(r)
	}
	r = callTool(t, tool, "runCommand", map[string]any{"command": "echo should-not-run", "timeout": -1})
	if r.Success {
		t.Fatal(r)
	}
	r = callTool(t, tool, "runCommand", map[string]any{"command": "printf completed", "run_in_background": true})
	assertOK(t, r)
	id := r.State.(map[string]any)["commandId"].(string)
	deadline := time.Now().Add(time.Second)
	for {
		r = callTool(t, tool, "getCommandOutput", map[string]any{"shell_id": id})
		assertOK(t, r)
		if r.State.(map[string]any)["running"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(r)
		}
		time.Sleep(time.Millisecond)
	}
	if r.State.(map[string]any)["exitCode"] != 0 || !strings.Contains(r.Content, "completed") {
		t.Fatal(r)
	}
}
