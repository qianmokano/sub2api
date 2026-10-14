package kanoagent

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

func syncTree(root string) error {
	directories := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		file, err := os.Open(directories[index])
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
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

func (e *Engine) storageBudget(ctx context.Context, snapshot *RuntimeSnapshot) (uint64, error) {
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
	var excludedBytes uint64
	for _, path := range postgresSources(snapshot) {
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
				excludedBytes += uint64(info.Size())
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if excludedBytes > sourceBytes {
		return 0, errors.New("ambiguous PostgreSQL mount size")
	}
	postgres := snapshot.Containers["postgres"]
	databaseValue, err := e.pg(ctx, postgres, "SELECT pg_database_size(current_database())")
	if err != nil {
		return 0, err
	}
	databaseBytes, err := strconv.ParseUint(databaseValue, 10, 64)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL storage size")
	}
	dumpValue, err := e.pg(ctx, postgres, "SELECT COALESCE(SUM(pg_table_size(c.oid)),0) FROM pg_class c JOIN pg_namespace n ON c.relnamespace=n.oid WHERE n.nspname='public' AND c.relkind='r'")
	if err != nil {
		return 0, err
	}
	dumpBytes, err := strconv.ParseUint(dumpValue, 10, 64)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL dump size")
	}
	// Cold source + non-PostgreSQL restore + fresh logical database + raw table
	// dump budget. Do not assume a compression ratio. Keep an extra 2 GiB.
	required := sourceBytes + (sourceBytes - excludedBytes) + databaseBytes + (512 << 20) + dumpBytes + (2 << 30)
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

func postgresSources(snapshot *RuntimeSnapshot) []string {
	roles := map[string]map[string]bool{}
	for service, container := range snapshot.Containers {
		for _, mount := range container.Mounts {
			if roles[mount.Source] == nil {
				roles[mount.Source] = map[string]bool{}
			}
			roles[mount.Source][service] = true
		}
	}
	result := []string{}
	for source, services := range roles {
		if len(services) == 1 && services["postgres"] {
			result = append(result, source)
		}
	}
	sort.Strings(result)
	unique := []string{}
	for _, source := range result {
		covered := false
		for _, parent := range unique {
			if inside(source, parent) {
				covered = true
			}
		}
		if !covered {
			unique = append(unique, source)
		}
	}
	return unique
}

// cloneFiltered preserves metadata but omits physical PostgreSQL data. Private
// PostgreSQL is built from the logical dump, not from the cold rollback copy.
func (e *Engine) cloneFiltered(ctx context.Context, root SavedRoot, excluded []string, target string) error {
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return errors.New("private restore destination already exists")
	}
	err := filepath.WalkDir(root.Saved, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root.Saved, path)
		if err != nil {
			return err
		}
		sourcePath := filepath.Join(root.Source, relative)
		for _, skip := range excluded {
			if inside(sourcePath, skip) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		destination := filepath.Join(target, relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		_, err = e.Runner.Run(ctx, []string{"cp", "-a", "--", path, destination}, nil, nil)
		return err
	})
	if err != nil {
		return err
	}
	// Apply directory permissions after copying children; readonly parents must
	// not prevent completing their private copy.
	keys := sortedKeys(root.Files)
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, relative := range keys {
		record := root.Files[relative]
		if record.Type != "directory" {
			continue
		}
		sourcePath := filepath.Join(root.Source, relative)
		skip := false
		for _, exclude := range excluded {
			if inside(sourcePath, exclude) {
				skip = true
			}
		}
		if skip {
			continue
		}
		destination := filepath.Join(target, relative)
		if err = os.Chown(destination, int(record.UID), int(record.GID)); err != nil {
			return err
		}
		if err = os.Chmod(destination, os.FileMode(record.Mode)); err != nil {
			return err
		}
	}
	actual, err := digestTree(target)
	if err != nil {
		return err
	}
	expected := map[string]FileRecord{}
	for relative, record := range root.Files {
		sourcePath := filepath.Join(root.Source, relative)
		skip := false
		for _, exclude := range excluded {
			if inside(sourcePath, exclude) {
				skip = true
			}
		}
		if !skip {
			expected[relative] = record
		}
	}
	if !reflect.DeepEqual(expected, actual) {
		return errors.New("private restored files differ from their frozen backup")
	}
	return nil
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
