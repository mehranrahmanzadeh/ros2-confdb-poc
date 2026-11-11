package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
)

var ErrStale = errors.New("configuration changed during refresh")

type Store struct {
	dir       string
	paramsDir string
}

func Open(common string) (*Store, error) {
	if !filepath.IsAbs(common) {
		return nil, fmt.Errorf("SNAP_COMMON must be an absolute path")
	}
	store := &Store{
		dir:       filepath.Join(common, ".agent"),
		paramsDir: filepath.Join(common, "params"),
	}
	if err := ensureDirectory(store.dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot secure agent state directory: %w", err)
	}
	if err := ensureDirectory(store.paramsDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot prepare parameters directory: %w", err)
	}
	return store, nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory")
	}
	return os.Chmod(path, mode)
}

func (store *Store) SocketPath() string {
	return filepath.Join(store.dir, "refresh.sock")
}

func (store *Store) LockDaemon() (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(store.dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open daemon lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("cannot acquire daemon lock: %w", err)
	}
	return file, nil
}

// CleanupTemporary must run while the daemon lock is held.
func (store *Store) CleanupTemporary() error {
	return store.locked(func() error {
		entries, err := os.ReadDir(store.dir)
		if err != nil {
			return fmt.Errorf("cannot inspect temporary state files: %w", err)
		}
		var failures []error
		removed := false
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".staged-") && !strings.HasPrefix(entry.Name(), ".state-") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				failures = append(failures, fmt.Errorf("cannot inspect temporary state file: %w", err))
				continue
			}
			if !info.Mode().IsRegular() {
				failures = append(failures, fmt.Errorf("temporary state path is not a regular file"))
				continue
			}
			if err := os.Remove(filepath.Join(store.dir, entry.Name())); err != nil {
				failures = append(failures, fmt.Errorf("cannot remove temporary state file: %w", err))
				continue
			}
			removed = true
		}
		if removed {
			if err := syncDirectory(store.dir); err != nil {
				failures = append(failures, fmt.Errorf("cannot sync temporary-file cleanup: %w", err))
			}
		}
		return errors.Join(failures...)
	})
}

func (store *Store) Generation() (uint64, error) {
	var generation uint64
	err := store.locked(func() error {
		var err error
		generation, err = store.readGeneration()
		return err
	})
	return generation, err
}

// Bump is called by the change-view hook before snapd commits a change.
func (store *Store) Bump() error {
	return store.locked(func() error {
		generation, err := store.readGeneration()
		if err != nil {
			return err
		}
		if generation == math.MaxUint64 {
			return fmt.Errorf("configuration generation is exhausted")
		}
		return store.writeStateFile("generation", []byte(strconv.FormatUint(generation+1, 10)+"\n"))
	})
}

func (store *Store) Publish(generation uint64, name string, data []byte) (err error) {
	if err := config.ValidateOutput(name); err != nil {
		return err
	}
	temp, err := os.CreateTemp(store.dir, ".staged-")
	if err != nil {
		return fmt.Errorf("cannot stage output: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		if removeErr := os.Remove(tempName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("cannot remove staged output: %w", removeErr))
		}
	}()
	if err := temp.Chmod(0644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot set staged output permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot write staged output: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot sync staged output: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("cannot close staged output: %w", err)
	}

	return store.locked(func() error {
		if err := store.requireGeneration(generation); err != nil {
			return err
		}
		managed, err := store.readManifest()
		if err != nil {
			return err
		}
		target := filepath.Join(store.paramsDir, name)
		info, statErr := os.Lstat(target)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("cannot inspect output %q: %w", name, statErr)
		}
		if statErr == nil {
			if !managed[name] {
				return fmt.Errorf("refusing to overwrite unmanaged output %q", name)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("managed output %q is not a regular file", name)
			}
		}
		if !managed[name] {
			managed[name] = true
			if err := store.writeManifest(managed); err != nil {
				return err
			}
		}
		if err := os.Rename(tempName, target); err != nil {
			return fmt.Errorf("cannot publish output %q: %w", name, err)
		}
		if err := syncDirectory(store.paramsDir); err != nil {
			return fmt.Errorf("cannot sync published output %q: %w", name, err)
		}
		return nil
	})
}

// RemoveUnconfigured removes only files recorded as managed by the agent.
func (store *Store) RemoveUnconfigured(generation uint64, desired map[string]bool) error {
	return store.locked(func() error {
		if err := store.requireGeneration(generation); err != nil {
			return err
		}
		managed, err := store.readManifest()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(managed))
		for name := range managed {
			if !desired[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)

		var failures []error
		changed := false
		for _, name := range names {
			path := filepath.Join(store.paramsDir, name)
			info, err := os.Lstat(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("cannot inspect removed output %q: %w", name, err))
				continue
			}
			if err == nil && !info.Mode().IsRegular() {
				failures = append(failures, fmt.Errorf("managed output %q is not a regular file", name))
				continue
			}
			if err == nil {
				if err := os.Remove(path); err != nil {
					failures = append(failures, fmt.Errorf("cannot remove output %q: %w", name, err))
					continue
				}
			}
			delete(managed, name)
			changed = true
		}
		if changed {
			if err := syncDirectory(store.paramsDir); err != nil {
				failures = append(failures, fmt.Errorf("cannot sync removed outputs: %w", err))
			}
			if err := store.writeManifest(managed); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	})
}

func (store *Store) requireGeneration(want uint64) error {
	current, err := store.readGeneration()
	if err != nil {
		return err
	}
	if current != want {
		return ErrStale
	}
	return nil
}

func (store *Store) readGeneration() (uint64, error) {
	data, err := os.ReadFile(filepath.Join(store.dir, "generation"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read configuration generation: %w", err)
	}
	generation, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid configuration generation: %w", err)
	}
	return generation, nil
}

func (store *Store) readManifest() (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(store.dir, "managed.json"))
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]bool), nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read managed-output manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var names []string
	if err := decoder.Decode(&names); err != nil || names == nil {
		return nil, fmt.Errorf("invalid managed-output manifest")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("invalid trailing managed-output manifest data")
	}
	managed := make(map[string]bool, len(names))
	for _, name := range names {
		if err := config.ValidateOutput(name); err != nil || managed[name] {
			return nil, fmt.Errorf("invalid or duplicate managed-output filename")
		}
		managed[name] = true
	}
	return managed, nil
}

func (store *Store) writeManifest(managed map[string]bool) error {
	names := make([]string, 0, len(managed))
	for name := range managed {
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("cannot encode managed-output manifest: %w", err)
	}
	if err := store.writeStateFile("managed.json", append(data, '\n')); err != nil {
		return fmt.Errorf("cannot save managed-output manifest: %w", err)
	}
	return nil
}

func (store *Store) writeStateFile(name string, data []byte) (err error) {
	temp, err := os.CreateTemp(store.dir, ".state-")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() {
		if removeErr := os.Remove(tempName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("cannot remove temporary state: %w", removeErr))
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filepath.Join(store.dir, name)); err != nil {
		return err
	}
	return syncDirectory(store.dir)
}

func (store *Store) locked(fn func() error) (err error) {
	file, err := os.OpenFile(filepath.Join(store.dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("cannot open agent state lock: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot close agent state lock: %w", closeErr))
		}
	}()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot lock agent state: %w", err)
	}
	defer func() {
		if unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot unlock agent state: %w", unlockErr))
		}
	}()
	return fn()
}

func syncDirectory(path string) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, dir.Close())
	}()
	return dir.Sync()
}
