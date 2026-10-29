package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/bmatcuk/doublestar/v4"
)

type tools struct {
	home, shell string
	mu          sync.Mutex
	commands    map[string]*command
	closed      bool
}

func newTools(home, shell string) *tools {
	return &tools{home: home, shell: shell, commands: make(map[string]*command)}
}

type args struct {
	Cwd             string            `json:"cwd"`
	Path            string            `json:"path"`
	DirectoryPath   string            `json:"directoryPath"`
	FilePath        string            `json:"file_path"`
	Content         *string           `json:"content"`
	Loc             []int             `json:"loc"`
	OldString       *string           `json:"old_string"`
	NewString       *string           `json:"new_string"`
	Search          *string           `json:"search"`
	Replace         *string           `json:"replace"`
	ReplaceAll      bool              `json:"replace_all"`
	All             bool              `json:"all"`
	Pattern         string            `json:"pattern"`
	Scope           string            `json:"scope"`
	Directory       string            `json:"directory"`
	Command         string            `json:"command"`
	Env             map[string]string `json:"env"`
	RunInBackground bool              `json:"run_in_background"`
	Background      bool              `json:"background"`
	ShellID         string            `json:"shell_id"`
	CommandID       string            `json:"commandId"`
	Timeout         *int64            `json:"timeout"`
	IgnoreCase      bool              `json:"-i"`
}

func (t *tools) resolve(cwd, path string) string {
	if cwd == "" {
		cwd = t.home
	}
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(t.home, cwd)
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(cwd, path)
}

func (t *tools) execute(ctx context.Context, call toolCall) toolResult {
	if call.Identifier != "lobe-local-system" || (call.Type != "" && call.Type != "tool") {
		return unsupported(call.Identifier + "/" + call.APIName)
	}
	names := map[string]string{"readLocalFile": "readFile", "writeLocalFile": "writeFile", "editLocalFile": "editFile", "listLocalFiles": "listFiles", "globLocalFiles": "globFiles"}
	name := call.APIName
	if n, ok := names[name]; ok {
		name = n
	}
	switch name {
	case "runCommand", "getCommandOutput", "killCommand", "readFile", "writeFile", "editFile", "listFiles", "grepContent", "globFiles":
	default:
		return unsupported(call.APIName)
	}
	var a args
	if err := json.Unmarshal([]byte(call.Arguments), &a); err != nil {
		return failure(fmt.Errorf("invalid arguments JSON: %w", err))
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &raw); err != nil || raw == nil {
		return failure(errors.New("arguments must be a JSON object"))
	}
	if option := disallowedOptions(raw); option != "" {
		return unsupported(option)
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	switch name {
	case "runCommand":
		return t.startCommand(ctx, a)
	case "getCommandOutput":
		return t.commandOutput(ctx, a, false)
	case "killCommand":
		return t.commandOutput(ctx, a, true)
	case "grepContent", "globFiles":
		return t.search(ctx, name, a)
	}
	path := a.Path
	if name == "editFile" && a.FilePath != "" {
		path = a.FilePath
	}
	if name == "listFiles" && path == "" {
		path = a.DirectoryPath
	}
	if path == "" && name != "listFiles" {
		return failure(errors.New("file path is required"))
	}
	path = t.resolve(a.Cwd, path)
	result, err := t.file(ctx, name, path, a)
	if err != nil {
		return failure(fmt.Errorf("%s %s: %w", name, path, err))
	}
	return result
}

func readText(ctx context.Context, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	// Never block the dispatcher on FIFOs, sockets, devices or procfs streams.
	if !info.Mode().IsRegular() {
		return "", errors.New("only regular files are supported")
	}
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if b.Len()+n > maxOutput {
			return "", errors.New("file exceeds 4 MiB read limit; use shell for larger files")
		}
		b.Write(buf[:n])
		if err == io.EOF {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
	}
}

func (t *tools) file(ctx context.Context, name, path string, a args) (toolResult, error) {
	state := map[string]any{"path": path}
	body := ""
	switch name {
	case "readFile":
		text, err := readText(ctx, path)
		if err != nil {
			return toolResult{}, err
		}
		totalChars := len([]rune(text))
		lines := strings.Split(text, "\n")
		if len(a.Loc) > 0 {
			if len(a.Loc) != 2 || a.Loc[0] < 0 || a.Loc[1] < a.Loc[0] {
				return toolResult{}, errors.New("loc must be [start, end], zero-based and end-exclusive")
			}
			text = strings.Join(lines[min(a.Loc[0], len(lines)):min(a.Loc[1], len(lines))], "\n")
		}
		state["content"] = text
		state["totalLines"] = len(lines)
		state["totalCharCount"] = totalChars
		if len(a.Loc) == 2 {
			state["loc"] = a.Loc
		}
		body = text
	case "writeFile":
		if a.Content == nil {
			return toolResult{}, errors.New("content is required")
		}
		if err := ctx.Err(); err != nil {
			return toolResult{}, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return toolResult{}, err
		}
		if err := writeText(ctx, path, *a.Content); err != nil {
			return toolResult{}, err
		}
		body = "Wrote file:\n" + *a.Content
		state["success"] = true
		state["bytesWritten"] = len(*a.Content)
	case "editFile":
		old, replacement := a.OldString, a.NewString
		if old == nil {
			old = a.Search
		}
		if replacement == nil {
			replacement = a.Replace
		}
		if old == nil || *old == "" || replacement == nil {
			return toolResult{}, errors.New("nonempty old_string and new_string are required")
		}
		text, err := readText(ctx, path)
		if err != nil {
			return toolResult{}, err
		}
		count := strings.Count(text, *old)
		if count == 0 {
			return toolResult{}, errors.New("old_string was not found; file unchanged")
		}
		if count > 1 && !a.ReplaceAll && !a.All {
			return toolResult{}, errors.New("old_string is ambiguous; use replace_all or provide unique context")
		}
		n := 1
		if a.ReplaceAll || a.All {
			n = -1
		}
		updated := strings.Replace(text, *old, *replacement, n)
		if err := writeText(ctx, path, updated); err != nil {
			return toolResult{}, err
		}
		body = "Updated file:\n" + updated
		if n == 1 {
			count = 1
		}
		state["replacements"] = count
	case "listFiles":
		entries, err := os.ReadDir(path)
		if err != nil {
			return toolResult{}, err
		}
		files := make([]map[string]any, 0, len(entries))
		var b strings.Builder
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return toolResult{}, err
			}
			info, err := e.Info()
			if err != nil {
				return toolResult{}, err
			}
			full := filepath.Join(path, e.Name())
			files = append(files, map[string]any{"name": e.Name(), "path": full, "isDirectory": e.IsDir(), "size": info.Size()})
			fmt.Fprintf(&b, "%s\tdirectory=%v\tsize=%d\n", full, e.IsDir(), info.Size())
			if b.Len() > maxOutput {
				return toolResult{}, errors.New("directory listing exceeds 4 MiB")
			}
		}
		state["files"], state["totalCount"] = files, len(files)
		body = b.String()
		if len(files) == 0 {
			body = "(empty directory)"
		}
	}
	return toolResult{Success: true, State: state, Content: "Path: " + path + "\n\n" + body}, nil
}

func writeText(ctx context.Context, path, content string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Open without truncation first, so unsupported special files cannot block or be damaged.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("only regular files are supported")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = io.Copy(f, strings.NewReader(content))
	return err
}

func (t *tools) search(ctx context.Context, name string, a args) toolResult {
	if a.Pattern == "" {
		return failure(errors.New("pattern is required"))
	}
	root := a.Path
	if root == "" {
		root = a.Scope
	}
	if root == "" {
		root = a.Directory
	}
	root = t.resolve(a.Cwd, root)
	var regex *regexp.Regexp
	if name == "grepContent" {
		pattern := a.Pattern
		if a.IgnoreCase {
			pattern = "(?i)" + pattern
		}
		var err error
		regex, err = regexp.Compile(pattern)
		if err != nil {
			return failure(fmt.Errorf("invalid pattern: %w", err))
		}
	} else if !doublestar.ValidatePattern(a.Pattern) {
		return failure(errors.New("invalid glob pattern"))
	}
	files := []string{}
	matches := []map[string]any{}
	var body strings.Builder
	skipped := 0
	// WalkDir does not follow directory symlinks, preventing cycles.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if name == "globFiles" {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.ToSlash(rel)
			if filepath.IsAbs(a.Pattern) {
				target = filepath.ToSlash(path)
			}
			ok, err := doublestar.Match(a.Pattern, target)
			if err != nil {
				return err
			}
			if ok {
				files = append(files, path)
				fmt.Fprintln(&body, path)
			}
		} else {
			if !d.Type().IsRegular() {
				return nil
			}
			text, err := readText(ctx, path)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				skipped++
				return nil
			}
			if strings.ContainsRune(text, 0) {
				skipped++
				return nil
			}
			scanner := bufio.NewScanner(strings.NewReader(text))
			scanner.Buffer(make([]byte, 4096), maxOutput+1)
			line := 0
			for scanner.Scan() {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				line++
				if regex.MatchString(scanner.Text()) {
					matches = append(matches, map[string]any{"path": path, "lineNumber": line, "content": scanner.Text()})
					fmt.Fprintf(&body, "%s:%d:%s\n", path, line, scanner.Text())
				}
				if body.Len() > maxOutput {
					return errors.New("search result exceeds 4 MiB; narrow the scope")
				}
			}
			if err := scanner.Err(); err != nil {
				return err
			}
		}
		if body.Len() > maxOutput {
			return errors.New("search result exceeds 4 MiB; narrow the scope")
		}
		return nil
	})
	if err != nil {
		return failure(fmt.Errorf("%s %s: %w", name, root, err))
	}
	state := map[string]any{"pattern": a.Pattern}
	if name == "globFiles" {
		state["files"], state["totalCount"] = files, len(files)
	} else {
		state["matches"], state["totalMatches"] = matches, len(matches)
	}
	if body.Len() == 0 {
		body.WriteString("(no matches)\n")
	}
	if skipped > 0 {
		fmt.Fprintf(&body, "Skipped %d unreadable, binary or oversized files.\n", skipped)
	}
	return toolResult{Success: true, State: state, Content: "Path: " + root + "\nPattern: " + a.Pattern + "\n\n" + body.String()}
}
