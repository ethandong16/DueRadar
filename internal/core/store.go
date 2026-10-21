package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type Engine struct {
	mu    sync.Mutex
	path  string
	state State
}

// Open creates or opens a private state file. Every process start requires a
// successful platform sync before saved task notifications may be delivered.
func Open(path string) (*Engine, error) {
	if path == "" {
		return nil, errors.New("state path is empty")
	}
	e := &Engine{path: path}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		e.state = State{Version: 1, Settings: DefaultSettings(), Tasks: []Task{}, Events: []Event{}, Accounts: []Account{}}
		for _, p := range []string{PlatformRain, PlatformZhidaoShared, PlatformZhidaoCampus} {
			e.state.Accounts = append(e.state.Accounts, Account{Platform: p, LoginState: "unbound", SyncState: "idle", NeedsRevalidation: true})
		}
		if err = e.save(e.state); err != nil {
			return nil, err
		}
		return e, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	if err = dec.Decode(&e.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("state has trailing or oversized data")
	}
	if e.state.Version != 1 {
		return nil, errors.New("unsupported state version")
	}
	if err = ValidateSettings(e.state.Settings); err != nil {
		return nil, fmt.Errorf("invalid stored settings: %w", err)
	}
	// Windows denies replacing a file while this read handle remains open.
	if err = f.Close(); err != nil {
		return nil, fmt.Errorf("close state: %w", err)
	}
	if err = e.RequireRevalidation(); err != nil {
		return nil, err
	}
	return e, nil
}

// Snapshot returns a deep copy; callers cannot modify scheduler state.
func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneState(e.state)
}

func cloneState(s State) State {
	// All data admitted into State is JSON serializable. This also copies all
	// slices and optional scalar pointers shared with adapters and HTTP callers.
	b, _ := json.Marshal(s)
	var copy State
	_ = json.Unmarshal(b, &copy)
	return copy
}

func (e *Engine) change(fn func(*State) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	next := cloneState(e.state)
	if err := fn(&next); err != nil {
		return err
	}
	next = cloneState(next)
	if err := e.save(next); err != nil {
		return err
	}
	e.state = next
	return nil
}

func (e *Engine) save(s State) error {
	dir := filepath.Dir(e.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".dueradar-*.tmp")
	if err != nil {
		return fmt.Errorf("create state transaction: %w", err)
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(s); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, e.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	if runtime.GOOS != "windows" {
		if d, openErr := os.Open(dir); openErr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}
