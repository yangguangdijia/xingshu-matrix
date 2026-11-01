package bastion

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const storageMarker = ".matrix-bastion-storage-id"

var recordingName = regexp.MustCompile(`^([1-9][0-9]*)-([a-f0-9]{32})\.cast$`)

// The marker identifies the backed-up volume, not a Pod's emptyDir. Checking
// mountinfo also prevents a configuration flag from blessing ephemeral storage.
func (m *Module) recordingReady() error {
	if m.storageID == "" {
		return errors.New("MATRIX_BASTION_RECORDING_STORAGE_ID is not configured")
	}
	root, err := safeRecordingRoot(m.recordDir)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("shared NFS recording mount cannot be verified on this platform")
	}
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return errors.New("cannot verify recording mount")
	}
	if !nfsMount(root, string(mounts)) {
		return errors.New("recording directory must be on a shared NFS mount")
	}
	f, err := openRegular(root, storageMarker)
	if err != nil {
		return errors.New("recording volume identity is unavailable")
	}
	defer f.Close()
	marker, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil || len(marker) > 256 || strings.TrimSpace(string(marker)) != m.storageID {
		return errors.New("recording volume identity mismatch")
	}
	return nil
}

func safeRecordingRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("recording directory is unavailable")
	}
	if filepath.Clean(real) != filepath.Clean(abs) {
		return "", errors.New("recording directory must not contain symlinks")
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", errors.New("invalid recording directory")
	}
	if st.Mode().Perm()&0o022 != 0 {
		return "", errors.New("recording directory must not be group or world writable")
	}
	return abs, nil
}

func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func nfsMount(dir, mountinfo string) bool {
	longest, shared := -1, false
	for _, line := range strings.Split(mountinfo, "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		left, right := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(left) < 6 || len(right) < 3 {
			continue
		}
		mount := filepath.Clean(unescapeMount(left[4]))
		rel, err := filepath.Rel(mount, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if len(mount) > longest {
			longest = len(mount)
			shared = (right[0] == "nfs" || right[0] == "nfs4") && !strings.Contains(","+left[5]+",", ",ro,")
		}
	}
	return shared
}

// Open the exact DB-bound file, reject links and serve from the checked
// descriptor (never hand a path back to an HTTP helper that reopens it).
func openRegular(root, name string) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\:`) {
		return nil, errors.New("invalid recording name")
	}
	path := filepath.Join(root, name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("recording is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, errors.New("recording file changed during open")
	}
	return f, nil
}

func (m *Module) openRecording(s *Session) (*os.File, error) {
	match := recordingName.FindStringSubmatch(s.RecordingPath)
	if len(match) != 3 || match[1] != itoa(s.ID) || match[2] != s.FenceToken ||
		s.RecordingStorageID != m.storageID || s.RecordingState != "ready" || s.Status == "active" {
		return nil, errors.New("recording is not finalized on this storage volume")
	}
	if err := m.recordingReady(); err != nil {
		return nil, err
	}
	root, err := safeRecordingRoot(m.recordDir)
	if err != nil {
		return nil, err
	}
	return openRegular(root, s.RecordingPath)
}

func (m *Module) createRecording(ref sessionRef, cols, rows int, title string) (*Recorder, string, error) {
	if err := m.recordingReady(); err != nil {
		return nil, "", err
	}
	root, err := safeRecordingRoot(m.recordDir)
	if err != nil {
		return nil, "", err
	}
	name := fmt.Sprintf("%d-%s.cast", ref.ID, ref.Fence)
	if !recordingName.MatchString(name) {
		return nil, "", errors.New("invalid recording identity")
	}
	rec, err := NewRecorder(filepath.Join(root, name), cols, rows, title)
	return rec, name, err
}
