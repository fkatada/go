// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix || windows || wasip1

package os

import (
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"
)

// root implementation for platforms with a function to open a file
// relative to a directory.
type root struct {
	name string

	// refs is incremented while an operation is using fd.
	// closed is set when Close is called.
	// fd is closed when closed is true and refs is 0.
	mu      sync.Mutex
	fd      sysfdType
	refs    int             // number of active operations
	closed  bool            // set when closed
	cleanup runtime.Cleanup // cleanup closes the file when no longer referenced
}

func (r *root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && r.refs == 0 {
		syscall.Close(r.fd)
	}
	r.closed = true
	// There is no need for a cleanup at this point. Root must be alive at the point
	// where cleanup.stop is called.
	r.cleanup.Stop()
	return nil
}

func (r *root) incref() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	r.refs++
	return nil
}

func (r *root) decref() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs <= 0 {
		panic("bad Root refcount")
	}
	r.refs--
	if r.closed && r.refs == 0 {
		syscall.Close(r.fd)
	}
}

func (r *root) Name() string {
	return r.name
}

func rootChmod(r *Root, name string, mode FileMode) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, chmodat(parent, name, mode)
	})
	if err != nil {
		return &PathError{Op: "chmodat", Path: name, Err: err}
	}
	return nil
}

func rootChown(r *Root, name string, uid, gid int) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, chownat(parent, name, uid, gid)
	})
	if err != nil {
		return &PathError{Op: "chownat", Path: name, Err: err}
	}
	return nil
}

func rootLchown(r *Root, name string, uid, gid int) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, lchownat(parent, name, uid, gid)
	})
	if err != nil {
		return &PathError{Op: "lchownat", Path: name, Err: err}
	}
	return err
}

func rootChtimes(r *Root, name string, atime time.Time, mtime time.Time) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, chtimesat(parent, name, atime, mtime)
	})
	if err != nil {
		return &PathError{Op: "chtimesat", Path: name, Err: err}
	}
	return err
}

func rootMkdir(r *Root, name string, perm FileMode) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, mkdirat(parent, name, perm)
	})
	if err != nil {
		return &PathError{Op: "mkdirat", Path: name, Err: err}
	}
	return nil
}

func rootReadlink(r *Root, name string) (string, error) {
	target, err := doInRoot(r, name, func(parent sysfdType, name string) (string, error) {
		return readlinkat(parent, name)
	})
	if err != nil {
		return "", &PathError{Op: "readlinkat", Path: name, Err: err}
	}
	return target, nil
}

func rootRemove(r *Root, name string) error {
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, removeat(parent, name)
	})
	if err != nil {
		return &PathError{Op: "removeat", Path: name, Err: err}
	}
	return nil
}

func rootRemoveAll(r *Root, name string) error {
	// Consistency with os.RemoveAll: Strip trailing /s from the name,
	// so RemoveAll("not_a_directory/") succeeds.
	for len(name) > 0 && IsPathSeparator(name[len(name)-1]) {
		name = name[:len(name)-1]
	}
	if endsWithDot(name) {
		// Consistency with os.RemoveAll: Return EINVAL when trying to remove .
		return &PathError{Op: "RemoveAll", Path: name, Err: syscall.EINVAL}
	}
	_, err := doInRoot(r, name, func(parent sysfdType, name string) (struct{}, error) {
		return struct{}{}, removeAllFrom(parent, name)
	})
	if IsNotExist(err) {
		return nil
	}
	if err != nil {
		return &PathError{Op: "RemoveAll", Path: name, Err: underlyingError(err)}
	}
	return err
}

func rootRename(r *Root, oldname, newname string) error {
	_, err := doInRoot(r, oldname, func(oldparent sysfdType, oldname string) (struct{}, error) {
		_, err := doInRoot(r, newname, func(newparent sysfdType, newname string) (struct{}, error) {
			return struct{}{}, renameat(oldparent, oldname, newparent, newname)
		})
		return struct{}{}, err
	})
	if err != nil {
		return &LinkError{"renameat", oldname, newname, err}
	}
	return err
}

func rootLink(r *Root, oldname, newname string) error {
	_, err := doInRoot(r, oldname, func(oldparent sysfdType, oldname string) (struct{}, error) {
		_, err := doInRoot(r, newname, func(newparent sysfdType, newname string) (struct{}, error) {
			return struct{}{}, linkat(oldparent, oldname, newparent, newname)
		})
		return struct{}{}, err
	})
	if err != nil {
		return &LinkError{"linkat", oldname, newname, err}
	}
	return err
}

// doInRoot performs an operation on a path in a Root.
//
// It opens the directory containing the final element of the path,
// and calls f with the directory FD and name of the final element.
//
// If the path refers to a symlink which should be followed,
// then f must return errSymlink.
// doInRoot will follow the symlink and call f again.
func doInRoot[T any](r *Root, name string, f func(parent sysfdType, name string) (T, error)) (ret T, err error) {
	if err := r.root.incref(); err != nil {
		return ret, err
	}
	defer r.root.decref()

	parts, suffixSep, err := splitPathInRoot(name, nil, nil)
	if err != nil {
		return ret, err
	}

	rootfd := r.root.fd
	dirfd := rootfd
	defer func() {
		if dirfd != rootfd {
			syscall.Close(dirfd)
		}
	}()

	// When resolving .. path components, we restart path resolution from the root.
	// (We can't openat(dir, "..") to move up to the parent directory,
	// because dir may have moved since we opened it.)
	// To limit how many opens a malicious path can cause us to perform, we set
	// a limit on the total number of path steps and the total number of restarts
	// caused by .. components. If *both* limits are exceeded, we halt the operation.
	const maxSteps = 255
	const maxRestarts = 8

	i := 0
	steps := 0
	restarts := 0
	symlinks := 0
	for {
		steps++
		if steps > maxSteps && restarts > maxRestarts {
			return ret, syscall.ENAMETOOLONG
		}

		if parts[i] == ".." {
			// Resolve one or more parent ("..") path components.
			//
			// Rewrite the original path,
			// removing the elements eliminated by ".." components,
			// and start over from the beginning.
			restarts++
			end := i + 1
			for end < len(parts) && parts[end] == ".." {
				end++
			}
			count := end - i
			if count > i {
				return ret, errPathEscapes
			}
			parts = slices.Delete(parts, i-count, end)
			if len(parts) == 0 {
				parts = []string{"."}
			}
			i = 0
			if dirfd != rootfd {
				syscall.Close(dirfd)
			}
			dirfd = rootfd
			continue
		}

		if i == len(parts)-1 {
			// This is the last path element.
			// Call f to decide what to do with it.
			// If f returns errSymlink, this element is a symlink
			// which should be followed.
			// suffixSep contains any trailing separator characters
			// which we rejoin to the final part at this time.
			ret, err = f(dirfd, parts[i]+suffixSep)
			if _, ok := err.(errSymlink); !ok {
				return ret, err
			}
		} else {
			var fd sysfdType
			fd, err = rootOpenDir(dirfd, parts[i])
			if err == nil {
				if dirfd != rootfd {
					syscall.Close(dirfd)
				}
				dirfd = fd
			} else if _, ok := err.(errSymlink); !ok {
				return ret, err
			}
		}

		if e, ok := err.(errSymlink); ok {
			symlinks++
			if symlinks > rootMaxSymlinks {
				return ret, syscall.ELOOP
			}
			newparts, newSuffixSep, err := splitPathInRoot(string(e), parts[:i], parts[i+1:])
			if err != nil {
				return ret, err
			}
			if i == len(parts)-1 {
				// suffixSep contains any trailing path separator characters
				// in the link target.
				// If we are replacing the remainder of the path, retain these.
				// If we're replacing some intermediate component of the path,
				// ignore them, since intermediate components must always be
				// directories.
				suffixSep = newSuffixSep
			}
			if len(newparts) < i || !slices.Equal(parts[:i], newparts[:i]) {
				// Some component in the path which we have already traversed
				// has changed. We need to restart parsing from the root.
				i = 0
				if dirfd != rootfd {
					syscall.Close(dirfd)
				}
				dirfd = rootfd
			}
			parts = newparts
			continue
		}

		i++
	}
}

// errSymlink reports that a file being operated on is actually a symlink,
// and the target of that symlink.
type errSymlink string

func (errSymlink) Error() string { panic("errSymlink is not user-visible") }
