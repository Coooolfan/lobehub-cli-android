package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxOutput = 4 << 20

type outputBuffer struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), maxOutput-len(b.data))
	b.data = append(b.data, p[:n]...)
	if n < len(p) {
		b.truncated = true
	}
	return len(p), nil
}
func (b *outputBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.data)
	if b.truncated {
		s += "\n[output truncated after 4 MiB]"
	}
	return s
}

type command struct {
	mu             sync.Mutex
	id             string
	cmd            *exec.Cmd
	stdout, stderr outputBuffer
	done           chan struct{}
	started        time.Time
	running        bool
	exitCode       *int
	err            string
}

func (p *command) kill(reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return nil
	}
	// Kill the shell's entire process group, including children that keep output pipes open.
	err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if reason != "" {
		p.err = reason
	}
	return nil
}

func (t *tools) startCommand(ctx context.Context, a args) toolResult {
	if a.Command == "" {
		return failure(errors.New("command is required"))
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if a.Timeout != nil && *a.Timeout <= 0 {
		return failure(errors.New("command timeout must be positive"))
	}
	p := &command{id: uuid(), done: make(chan struct{}), started: time.Now(), running: true}
	p.cmd = exec.Command(t.shell, "-c", a.Command)
	p.cmd.Dir = t.resolve(a.Cwd, "")
	p.cmd.Env = os.Environ()
	for key, value := range a.Env {
		if key == "HOME" {
			continue
		} // Keep the writable HOME selected at startup.
		p.cmd.Env = append(p.cmd.Env, key+"="+value)
	}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	// Bound Wait when descendants detach from the shell's group and retain its pipes.
	p.cmd.WaitDelay = 250 * time.Millisecond
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return failure(errors.New("client is stopping"))
	}
	if err := p.cmd.Start(); err != nil {
		t.mu.Unlock()
		return failure(fmt.Errorf("start shell in %s: %w", p.cmd.Dir, err))
	}
	t.commands[p.id] = p
	t.mu.Unlock()
	go func() {
		err := p.cmd.Wait()
		if errors.Is(err, exec.ErrWaitDelay) {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		p.mu.Lock()
		code := p.cmd.ProcessState.ExitCode()
		p.exitCode = &code
		p.running = false
		if err != nil && p.err == "" {
			p.err = err.Error()
		}
		p.mu.Unlock()
		close(p.done)
	}()
	deadline, ok := ctx.Deadline()
	if a.Timeout != nil {
		d := time.Now().Add(milliseconds(*a.Timeout))
		if !ok || d.Before(deadline) {
			deadline, ok = d, true
		}
	}
	// Background commands survive transport disconnect, but not the original execution deadline.
	if ok {
		go func() {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = p.kill("command timed out")
			case <-p.done:
			}
		}()
	}
	bg := a.Background || a.RunInBackground
	if bg {
		return p.result(true)
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		_ = p.kill("command timed out or canceled")
		<-p.done
	}
	return p.result(false)
}

func (p *command) result(background bool) toolResult {
	p.mu.Lock()
	state := map[string]any{"commandId": p.id, "isBackground": background, "running": p.running, "success": p.err == "", "stdout": p.stdout.String(), "stderr": p.stderr.String()}
	if p.exitCode != nil {
		state["exitCode"] = *p.exitCode
	}
	if p.err != "" {
		state["error"] = p.err
	}
	p.mu.Unlock()
	code := "pending"
	if v, ok := state["exitCode"]; ok {
		code = fmt.Sprint(v)
	}
	header := "Command completed successfully."
	if state["running"] == true {
		header = "Command is still running."
	}
	if state["success"] == false {
		header = "Command failed: " + fmt.Sprint(state["error"])
	}
	content := fmt.Sprintf("%s\nshell_id: %s\nexit_code: %s\nrunning: %v\n\nStdout:\n%s\n\nStderr:\n%s", header, p.id, code, state["running"], state["stdout"], state["stderr"])
	if state["stdout"] == "" && state["stderr"] == "" {
		content += "\n(no output)"
	}
	return toolResult{Success: state["success"].(bool), State: state, Content: content, Error: optionalError(state)}
}
func optionalError(state map[string]any) string {
	if s, ok := state["error"].(string); ok {
		return s
	}
	return ""
}

func (t *tools) commandOutput(ctx context.Context, a args, kill bool) toolResult {
	id := a.ShellID
	if id == "" {
		id = a.CommandID
	}
	t.mu.Lock()
	p := t.commands[id]
	t.mu.Unlock()
	if p == nil {
		return failure(fmt.Errorf("unknown shell_id: %s", id))
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if kill {
		if err := p.kill("killed by killCommand"); err != nil {
			return failure(err)
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return failure(ctx.Err())
		}
		return toolResult{Success: true, Content: "Command stopped.\n\n" + p.result(false).Content, State: map[string]any{"commandId": id, "success": true}}
	}
	r := p.result(false)
	state := r.State.(map[string]any)
	delete(state, "isBackground")
	delete(state, "commandId")
	state["durationMs"] = time.Since(p.started).Milliseconds()
	// Retrieving an exit failure is a successful observation; keep the lifecycle and error visible.
	r.Success = true
	state["success"] = true
	return r
}

func (t *tools) close() {
	t.mu.Lock()
	t.closed = true
	commands := make([]*command, 0, len(t.commands))
	for _, p := range t.commands {
		commands = append(commands, p)
	}
	t.mu.Unlock()
	for _, p := range commands {
		_ = p.kill("client stopped")
	}
	for _, p := range commands {
		<-p.done
	}
}

// Requests for sandboxing or a PTY must be refused, never silently downgraded.
func disallowedOptions(raw map[string]any) string {
	for k, v := range raw {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "sandbox") || strings.Contains(lower, "pty") || lower == "terminal" {
			if v != nil && v != false && v != "" {
				return k
			}
		}
	}
	return ""
}
