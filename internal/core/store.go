package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	mu          sync.Mutex
	path        string
	state       State
	backupError string
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
	// Keep one rolling copy of the last readable state before migrations or new
	// writes. A failed backup never blocks startup, but it is reported in status.
	if err := backupState(path); err != nil {
		e.backupError = "备份状态文件失败：" + err.Error()
	}
	// Zhidao poll monitoring is on by default, but a stored false is an explicit
	// opt-out. Only states written before the setting existed lack the key, so
	// probe the raw settings object to tell "never stored" from "stored false".
	missingZhidaoSetting := false
	if _, seekErr := f.Seek(0, io.SeekStart); seekErr == nil {
		var probe struct {
			Settings map[string]json.RawMessage `json:"settings"`
		}
		if json.NewDecoder(io.LimitReader(f, 64<<20)).Decode(&probe) == nil {
			_, stored := probe.Settings["zhidao_in_class_enabled"]
			missingZhidaoSetting = !stored
		}
	}
	// Windows denies replacing a file while this read handle remains open, so
	// close it before the one-time migration write below. The deferred Close
	// still covers every earlier error return and is harmless after this point.
	if err = f.Close(); err != nil {
		return nil, fmt.Errorf("close state: %w", err)
	}
	// Settings gained fields after the first state format. A missing poll
	// interval or timezone marks an older file, which is migrated once.
	migrate := false
	if missingZhidaoSetting {
		e.state.Settings.ZhidaoInClassEnabled = true
		migrate = true
	}
	if e.state.Settings.InClassPollSeconds == 0 {
		e.state.Settings.InClassEnabled = true
		e.state.Settings.InClassPollSeconds = DefaultInClassPollSeconds
		migrate = true
	}
	if strings.TrimSpace(e.state.Settings.Timezone) == "" {
		e.state.Settings.Timezone = "Asia/Shanghai"
		migrate = true
	}
	if migrate {
		if err = e.save(e.state); err != nil {
			return nil, fmt.Errorf("migrate state: %w", err)
		}
	}
	if err = ValidateSettings(e.state.Settings); err != nil {
		return nil, fmt.Errorf("invalid stored settings: %w", err)
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

// BackupError reports a failed rolling state backup so the status view can
// surface it without turning a read-only or full disk into a startup failure.
func (e *Engine) BackupError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.backupError
}

// StateSize reports the on-disk state size for growth monitoring. The file is
// replaced atomically, so a stat without the engine lock cannot observe a
// partially written snapshot.
func (e *Engine) StateSize() int64 {
	info, err := os.Stat(e.path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// backupState refreshes state.json.bak at most once per day. The copy is taken
// from the already validated file on disk, so it can never contain a state this
// process refused to load.
func backupState(path string) error {
	backup := path + ".bak"
	if info, err := os.Stat(backup); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(backup)
	f, err := os.CreateTemp(dir, ".dueradar-backup-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
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
	return os.Rename(name, backup)
}

func cloneState(s State) State {
	// Copy without serialization: invalid adapter timestamps must reach save's
	// validation error, never turn a transaction into an empty, valid JSON state.
	copy := s
	copy.Tasks = slices.Clone(s.Tasks)
	for i := range copy.Tasks {
		copy.Tasks[i].StartAt = cloneTime(s.Tasks[i].StartAt)
		copy.Tasks[i].EndAt = cloneTime(s.Tasks[i].EndAt)
		copy.Tasks[i].DismissedAt = cloneTime(s.Tasks[i].DismissedAt)
		if s.Tasks[i].Progress != nil {
			value := *s.Tasks[i].Progress
			copy.Tasks[i].Progress = &value
		}
	}
	copy.Events = slices.Clone(s.Events)
	for i := range copy.Events {
		copy.Events[i].SentAt = cloneTime(s.Events[i].SentAt)
		copy.Events[i].AnchorAt = cloneTime(s.Events[i].AnchorAt)
		copy.Events[i].NextAttemptAt = cloneTime(s.Events[i].NextAttemptAt)
		copy.Events[i].BlockedUntil = cloneTime(s.Events[i].BlockedUntil)
	}
	copy.Accounts = slices.Clone(s.Accounts)
	for i := range copy.Accounts {
		copy.Accounts[i].LastSuccessAt = cloneTime(s.Accounts[i].LastSuccessAt)
		copy.Accounts[i].LastAttemptAt = cloneTime(s.Accounts[i].LastAttemptAt)
	}
	copy.Settings.DeadlineMinutes = slices.Clone(s.Settings.DeadlineMinutes)
	copy.Settings.ExamStartMinutes = slices.Clone(s.Settings.ExamStartMinutes)
	copy.Settings.VideoMinutes = slices.Clone(s.Settings.VideoMinutes)
	if s.InClassSeen != nil {
		copy.InClassSeen = make(map[string]time.Time, len(s.InClassSeen))
		for key, seen := range s.InClassSeen {
			copy.InClassSeen[key] = seen
		}
	}
	if s.ClassroomActions != nil {
		copy.ClassroomActions = make(map[string]time.Time, len(s.ClassroomActions))
		for key, at := range s.ClassroomActions {
			copy.ClassroomActions[key] = at
		}
	}
	return copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePtr(*value)
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
	// Refresh the daily snapshot after a durable commit. If no state changes,
	// the existing backup is still the latest state. A backup failure must not
	// fail the write; it is surfaced in the status view.
	if err := backupState(e.path); err != nil {
		e.backupError = "备份状态文件失败：" + err.Error()
	} else {
		e.backupError = ""
	}
	return nil
}
