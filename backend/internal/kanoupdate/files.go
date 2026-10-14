package kanoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

type FileRecord struct {
	Type   string `json:"type"`
	Digest string `json:"digest,omitempty"`
	Link   string `json:"link,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
}

type SavedRoot struct {
	Source string                `json:"source"`
	Saved  string                `json:"saved"`
	Files  map[string]FileRecord `json:"files"`
}

func digestTree(root string) (map[string]FileRecord, error) {
	result := map[string]FileRecord{}
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("file ownership is unavailable")
		}
		record := FileRecord{Mode: uint32(info.Mode()), UID: stat.Uid, GID: stat.Gid}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			record.Type = "link"
			record.Link, err = os.Readlink(path)
		case info.IsDir():
			record.Type = "directory"
		case info.Mode().IsRegular():
			record.Type = "file"
			record.Size = info.Size()
			var file *os.File
			file, err = os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			_, err = io.Copy(hash, file)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			record.Digest = hex.EncodeToString(hash.Sum(nil))
		default:
			return errors.New("unsupported special file in deployment data")
		}
		if err != nil {
			return err
		}
		result[relative] = record
		return nil
	})
	return result, err
}

func atomicFile(path string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".updater-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(body)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func writePrivateJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(path, body, 0o600)
}

func copyRoot(ctx context.Context, runner Runner, source, target string) error {
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("restore destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	_, err := runner.Run(ctx, []string{"cp", "-a", "--", source, target}, nil, nil)
	return err
}

func verifyRoot(root SavedRoot) error {
	actual, err := digestTree(root.Saved)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(root.Files, actual) {
		return errors.New("backup files or ownership differ from their original data")
	}
	return nil
}

func (e *Engine) storageBudget(snapshot *RuntimeSnapshot) (uint64, error) {
	paths := []string{e.Config.DeploymentDir}
	for _, container := range snapshot.Containers {
		for _, mount := range container.Mounts {
			if e.managedMount(mount) || inside(mount.Source, e.Config.DeploymentDir) {
				continue
			}
			exists := false
			for _, path := range paths {
				if inside(mount.Source, path) {
					exists = true
				}
			}
			if !exists {
				paths = append(paths, mount.Source)
			}
		}
	}
	var sourceBytes uint64
	for _, path := range paths {
		err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			info, statErr := entry.Info()
			if errors.Is(statErr, os.ErrNotExist) {
				return nil
			}
			if statErr != nil {
				return statErr
			}
			if info.Mode().IsRegular() {
				sourceBytes += uint64(info.Size())
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	// Full cold backup, working restored copies and logical dump, plus 2 GiB.
	// This intentionally overestimates rather than relying on compressed dumps.
	required := sourceBytes*3 + 2*(1<<30)
	var stat unix.Statfs_t
	if err := unix.Statfs(e.Config.DeploymentDir, &stat); err != nil {
		return 0, err
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available < required {
		return required, errors.New("insufficient disk space for a complete backup and isolated restore")
	}
	return required, nil
}

func (e *Engine) saveFiles(ctx context.Context, state *UpgradeState) error {
	snapshot := state.Snapshot
	paths := []string{e.Config.DeploymentDir}
	for _, container := range snapshot.Containers {
		for _, mount := range container.Mounts {
			if e.managedMount(mount) || inside(mount.Source, e.Config.DeploymentDir) {
				continue
			}
			paths = append(paths, mount.Source)
		}
	}
	sort.Strings(paths)
	unique := []string{}
	for _, path := range paths {
		covered := false
		for _, parent := range unique {
			if inside(path, parent) {
				covered = true
				break
			}
		}
		if !covered {
			unique = append(unique, path)
		}
	}
	for index, source := range unique {
		name := "extra-" + strconv.Itoa(index)
		if source == e.Config.DeploymentDir {
			name = "original"
		}
		saved := filepath.Join(state.BackupPath, name)
		before, err := digestTree(source)
		if err != nil {
			return err
		}
		if err = copyRoot(ctx, e.Runner, source, saved); err != nil {
			return err
		}
		root := SavedRoot{Source: source, Saved: saved, Files: before}
		if err = verifyRoot(root); err != nil {
			return err
		}
		snapshot.Roots = append(snapshot.Roots, root)
	}
	for _, container := range snapshot.Containers {
		for _, mount := range container.Mounts {
			if e.managedMount(mount) {
				continue
			}
			for _, root := range snapshot.Roots {
				if inside(mount.Source, root.Source) {
					relative, _ := filepath.Rel(root.Source, mount.Source)
					snapshot.MountBackups[mount.Source] = filepath.Join(root.Saved, relative)
					break
				}
			}
			if snapshot.MountBackups[mount.Source] == "" {
				return errors.New("data mount was not included in the complete backup")
			}
		}
	}
	return writePrivateJSON(filepath.Join(state.BackupPath, "files.json"), snapshot.Roots)
}
