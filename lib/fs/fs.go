package fs

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/fasttime"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/filestream"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

var tmpFileNum uint64

// MustSyncPath syncs contents of the given path.
func MustSyncPath(path string) {
	mustSyncPath(path)
}

// WriteFileAtomically atomically writes data to the given file path.
//
// WriteFileAtomically returns only after the file is fully written and synced
// to the underlying storage.
func WriteFileAtomically(path string, data []byte) error {
	// Check for the existing file. It is expected that
	// the WriteFileAtomically function cannot be called concurrently
	// with the same `path`.
	if IsPathExist(path) {
		return fmt.Errorf("cannot create file %q, since it already exists", path)
	}

	n := atomic.AddUint64(&tmpFileNum, 1)
	tmpPath := fmt.Sprintf("%s.tmp.%d", path, n)
	f, err := filestream.Create(tmpPath, false)
	if err != nil {
		return fmt.Errorf("cannot create file %q: %w", tmpPath, err)
	}
	if _, err := f.Write(data); err != nil {
		f.MustClose()
		MustRemoveAll(tmpPath)
		return fmt.Errorf("cannot write %d bytes to file %q: %w", len(data), tmpPath, err)
	}

	// Sync and close the file.
	f.MustClose()

	// Atomically move the file from tmpPath to path.
	if err := os.Rename(tmpPath, path); err != nil {
		// do not call MustRemoveAll(tmpPath) here, so the user could inspect
		// the file contents during investigating the issue.
		return fmt.Errorf("cannot move %q to %q: %w", tmpPath, path, err)
	}

	// Sync the containing directory, so the file is guaranteed to appear in the directory.
	// See https://www.quora.com/When-should-you-fsync-the-containing-directory-in-addition-to-the-file-itself
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot obtain absolute path to %q: %w", path, err)
	}
	parentDirPath := filepath.Dir(absPath)
	MustSyncPath(parentDirPath)

	return nil
}

// IsTemporaryFileName returns true if fn matches temporary file name pattern
// from WriteFileAtomically.
func IsTemporaryFileName(fn string) bool {
	return tmpFileNameRe.MatchString(fn)
}

// tmpFileNameRe is regexp for temporary file name - see WriteFileAtomically for details.
var tmpFileNameRe = regexp.MustCompile(`\.tmp\.\d+$`)

// MkdirAllIfNotExist creates the given path dir if it isn't exist.
func MkdirAllIfNotExist(path string) error {
	if IsPathExist(path) {
		return nil
	}
	return mkdirSync(path)
}

// MkdirAllFailIfExist creates the given path dir if it isn't exist.
//
// Returns error if path already exists.
func MkdirAllFailIfExist(path string) error {
	if IsPathExist(path) {
		return fmt.Errorf("the %q already exists", path)
	}
	return mkdirSync(path)
}

func mkdirSync(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	// Sync the parent directory, so the created directory becomes visible
	// in the fs after power loss.
	parentDirPath := filepath.Dir(path)
	MustSyncPath(parentDirPath)
	return nil
}

// RemoveDirContents removes all the contents of the given dir if it exists.
//
// It doesn't remove the dir itself, so the dir may be mounted
// to a separate partition.
func RemoveDirContents(dir string) {
	if !IsPathExist(dir) {
		// The path doesn't exist, so nothing to remove.
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		logger.Panicf("FATAL: cannot open dir %q: %s", dir, err)
	}
	defer MustClose(d)
	names, err := d.Readdirnames(-1)
	if err != nil {
		logger.Panicf("FATAL: cannot read contents of the dir %q: %s", dir, err)
	}
	for _, name := range names {
		if name == "." || name == ".." || name == "lost+found" {
			// Skip special dirs.
			continue
		}
		fullPath := dir + "/" + name
		MustRemoveAll(fullPath)
	}
	MustSyncPath(dir)
}

// MustClose must close the given file f.
func MustClose(f *os.File) {
	fname := f.Name()
	if err := f.Close(); err != nil {
		logger.Panicf("FATAL: cannot close %q: %s", fname, err)
	}
}

// MustFileSize returns file size for the given path.
func MustFileSize(path string) uint64 {
	fi, err := os.Stat(path)
	if err != nil {
		logger.Panicf("FATAL: cannot stat %q: %s", path, err)
	}
	if fi.IsDir() {
		logger.Panicf("FATAL: %q must be a file, not a directory", path)
	}
	return uint64(fi.Size())
}

// IsPathExist returns whether the given path exists.
func IsPathExist(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		logger.Panicf("FATAL: cannot stat %q: %s", path, err)
	}
	return true
}

func mustSyncParentDirIfExists(path string) {
	parentDirPath := filepath.Dir(path)
	if !IsPathExist(parentDirPath) {
		return
	}
	MustSyncPath(parentDirPath)
}

// IsEmptyDir returns true if path points to empty directory.
func IsEmptyDir(path string) bool {
	// See https://stackoverflow.com/a/30708914/274937
	f, err := os.Open(path)
	if err != nil {
		logger.Panicf("FATAL: unexpected error when opening directory %q: %s", path, err)
	}
	_, err = f.Readdirnames(1)
	MustClose(f)
	if err != nil {
		if err == io.EOF {
			return true
		}
		logger.Panicf("FATAL: unexpected error when reading directory %q: %s", path, err)
	}
	return false
}

// MustRemoveAll removes path with all the contents.
//
// It properly fsyncs the parent directory after path removal.
//
// It properly handles NFS issue https://github.com/VictoriaMetrics/VictoriaMetrics/issues/61 .
func MustRemoveAll(path string) {
	mustRemoveAll(path, func() {})
}

// MustRemoveAllWithDoneCallback removes path with all the contents.
//
// It properly fsyncs the parent directory after path removal.
//
// done is called after the path is successfully removed.
//
// done may be called after the function returns for NFS path.
// See https://github.com/VictoriaMetrics/VictoriaMetrics/issues/61.
func MustRemoveAllWithDoneCallback(path string, done func()) {
	mustRemoveAll(path, done)
}

// HardLinkFiles makes hard links for all the files from srcDir in dstDir.
func HardLinkFiles(srcDir, dstDir string) error {
	if err := mkdirSync(dstDir); err != nil {
		return fmt.Errorf("cannot create dstDir=%q: %w", dstDir, err)
	}

	d, err := os.Open(srcDir)
	if err != nil {
		return fmt.Errorf("cannot open srcDir=%q: %w", srcDir, err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			logger.Panicf("FATAL: cannot close %q: %s", srcDir, err)
		}
	}()

	fis, err := d.Readdir(-1)
	if err != nil {
		return fmt.Errorf("cannot read files in scrDir=%q: %w", srcDir, err)
	}
	for _, fi := range fis {
		if IsDirOrSymlink(fi) {
			// Skip directories.
			continue
		}
		fn := fi.Name()
		srcPath := srcDir + "/" + fn
		dstPath := dstDir + "/" + fn
		if err := os.Link(srcPath, dstPath); err != nil {
			return err
		}
	}

	MustSyncPath(dstDir)
	return nil
}

// IsDirOrSymlink returns true if fi is directory or symlink.
func IsDirOrSymlink(fi os.FileInfo) bool {
	return fi.IsDir() || (fi.Mode()&os.ModeSymlink == os.ModeSymlink)
}

// SymlinkRelative creates relative symlink for srcPath in dstPath.
func SymlinkRelative(srcPath, dstPath string) error {
	baseDir := filepath.Dir(dstPath)
	srcPathRel, err := filepath.Rel(baseDir, srcPath)
	if err != nil {
		return fmt.Errorf("cannot make relative path for srcPath=%q: %w", srcPath, err)
	}
	return os.Symlink(srcPathRel, dstPath)
}

// CopyDirectory copies all the files in srcPath to dstPath.
func CopyDirectory(srcPath, dstPath string) error {
	des, err := os.ReadDir(srcPath)
	if err != nil {
		return err
	}
	if err := MkdirAllIfNotExist(dstPath); err != nil {
		return err
	}
	for _, de := range des {
		if !de.Type().IsRegular() {
			// Skip non-files
			continue
		}
		src := filepath.Join(srcPath, de.Name())
		dst := filepath.Join(dstPath, de.Name())
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	MustSyncPath(dstPath)
	return nil
}

func copyFile(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer MustClose(src)
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer MustClose(dst)
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	MustSyncPath(dstPath)
	return nil
}

// ReadFullData reads len(data) bytes from r.
func ReadFullData(r io.Reader, data []byte) error {
	n, err := io.ReadFull(r, data)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		return fmt.Errorf("cannot read %d bytes; read only %d bytes; error: %w", len(data), n, err)
	}
	if n != len(data) {
		logger.Panicf("BUG: io.ReadFull read only %d bytes; must read %d bytes", n, len(data))
	}
	return nil
}

// MustWriteData writes data to w.
func MustWriteData(w io.Writer, data []byte) {
	if len(data) == 0 {
		return
	}
	n, err := w.Write(data)
	if err != nil {
		logger.Panicf("FATAL: cannot write %d bytes: %s", len(data), err)
	}
	if n != len(data) {
		logger.Panicf("BUG: writer wrote %d bytes instead of %d bytes", n, len(data))
	}
}

// CreateFlockFile creates flock.lock file in the directory dir
// and returns the handler to the file.
func CreateFlockFile(dir string) (*os.File, error) {
	flockFile := dir + "/flock.lock"
	return createFlockFile(flockFile)
}

// MustGetFreeSpace returns free space for the given directory path.
func MustGetFreeSpace(path string) uint64 {
	// Try obtaining cached value at first.
	freeSpaceMapLock.Lock()
	defer freeSpaceMapLock.Unlock()

	e, ok := freeSpaceMap[path]
	if ok && fasttime.UnixTimestamp()-e.updateTime < 2 {
		// Fast path - the entry is fresh.
		return e.freeSpace
	}

	// Slow path.
	// Determine the amount of free space at path.
	e.freeSpace = mustGetFreeSpace(path)
	e.updateTime = fasttime.UnixTimestamp()
	freeSpaceMap[path] = e
	return e.freeSpace
}

var (
	freeSpaceMap     = make(map[string]freeSpaceEntry)
	freeSpaceMapLock sync.Mutex
)

type freeSpaceEntry struct {
	updateTime uint64
	freeSpace  uint64
}

// ReadFileOrHTTP reads path either from local filesystem or from http if path starts with http or https.
func ReadFileOrHTTP(path string) ([]byte, error) {
	if isHTTPURL(path) {
		// reads remote file via http or https, if url is given
		resp, err := http.Get(path)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch %q: %w", path, err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("cannot read %q: %s", path, err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %q: %w", path, err)
	}
	return data, nil
}

// GetFilepath returns full path to file for the given baseDir and path.
func GetFilepath(baseDir, path string) string {
	if filepath.IsAbs(path) || isHTTPURL(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

// isHTTPURL checks if a given targetURL is valid and contains a valid http scheme
func isHTTPURL(targetURL string) bool {
	parsed, err := url.Parse(targetURL)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""

}
