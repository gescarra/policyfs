package fuse

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hieutdo/policyfs/internal/errkind"
	"github.com/hieutdo/policyfs/internal/eventlog"
	"github.com/hieutdo/policyfs/internal/router"
	"github.com/rs/zerolog"
)

// Unlink removes a child file from every read target that has it.
func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	if n == nil {
		return toErrno(&errkind.NilError{What: "node"})
	}
	rt, log := n.runtime()
	if rt == nil {
		return toErrno(&errkind.NilError{What: "router"})
	}

	caller, callerOK := gofuse.FromContext(ctx)
	if !callerOK {
		return syscall.EPERM
	}

	parentVirtualPath := n.Path(n.Root())
	if parentVirtualPath == "." {
		parentVirtualPath = ""
	}
	virtualPath, errno := joinVirtualPath(parentVirtualPath, name)
	if errno != 0 {
		return errno
	}

	targets, err := rt.ResolveReadTargets(virtualPath)
	if err != nil {
		return toErrno(err)
	}
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Msg("unlink resolved targets")

	removed := false
	for _, t := range targets {
		ok, errno := n.unlinkOnTarget(ctx, log, caller, t, parentVirtualPath, virtualPath)
		if errno == syscall.EISDIR && removed {
			continue
		}
		if errno != 0 {
			return errno
		}
		removed = removed || ok
	}

	if removed {
		return 0
	}
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Msg("unlink missed on all targets")
	return syscall.ENOENT
}

// unlinkOnTarget removes a file from one target and reports whether it existed there.
func (n *Node) unlinkOnTarget(ctx context.Context, log zerolog.Logger, caller *gofuse.Caller, t router.Target, parentVirtualPath string, virtualPath string) (bool, syscall.Errno) {
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", t.Indexed).Msg("unlink scanning target")
	if !t.Indexed {
		physicalPath := filepath.Join(t.Root, virtualPath)
		st := syscall.Stat_t{}
		if err := syscall.Lstat(physicalPath, &st); err != nil {
			if errors.Is(err, syscall.ENOENT) {
				log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Str("real_path", physicalPath).Msg("unlink missed on non-indexed target")
				return false, 0
			}
			return false, toErrno(err)
		}
		if uint32(st.Mode)&syscall.S_IFMT == syscall.S_IFDIR {
			log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Str("real_path", physicalPath).Msg("unlink rejected directory on non-indexed target")
			return false, syscall.EISDIR
		}
		parentPhysical := filepath.Dir(physicalPath)
		pst := syscall.Stat_t{}
		if err := syscall.Lstat(parentPhysical, &pst); err != nil {
			return false, toErrno(err)
		}
		if uint32(pst.Mode)&syscall.S_IFMT != syscall.S_IFDIR {
			return false, syscall.ENOTDIR
		}
		if errno := dirWriteExecPermErrno(caller, uint32(pst.Mode), pst.Uid, pst.Gid); errno != 0 {
			return false, errno
		}
		if errno := stickyDirMayRemoveErrno(caller, uint32(pst.Mode), pst.Uid, st.Uid); errno != 0 {
			return false, errno
		}
		if errno := toErrno(syscall.Unlink(physicalPath)); errno != 0 {
			log.Error().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Err(errno).Msg("failed to unlink")
			return false, errno
		}
		log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", false).Msg("unlink")
		return true, 0
	}

	if n.db == nil {
		log.Error().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("failed to unlink: db is nil for indexed target")
		return false, syscall.EIO
	}
	f, ok, err := n.db.GetEffectiveFile(ctx, t.ID, virtualPath)
	if err != nil {
		return false, toErrno(fmt.Errorf("failed to lookup indexed file: %w", err))
	}
	if !ok {
		log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("unlink missed on indexed target")
		return false, 0
	}
	if f.IsDir {
		log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("unlink rejected directory on indexed target")
		return false, syscall.EISDIR
	}
	if parentVirtualPath != "" {
		pdir, ok, err := n.db.GetEffectiveFile(ctx, t.ID, parentVirtualPath)
		if err != nil {
			return false, toErrno(fmt.Errorf("failed to lookup indexed parent dir: %w", err))
		}
		if !ok {
			return false, syscall.ENOENT
		}
		if !pdir.IsDir {
			return false, syscall.ENOTDIR
		}
		if errno := dirWriteExecPermErrno(caller, pdir.Mode, pdir.UID, pdir.GID); errno != 0 {
			return false, errno
		}
		if errno := stickyDirMayRemoveErrno(caller, pdir.Mode, pdir.UID, f.UID); errno != 0 {
			return false, errno
		}
	}
	updated, err := n.db.MarkDeleted(ctx, t.ID, virtualPath, false)
	if err != nil {
		return false, toErrno(err)
	}
	if !updated {
		log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("unlink indexed metadata update missed")
		return false, 0
	}
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("unlink updated indexed metadata")
	if err := eventlog.Append(ctx, n.mountName, eventlog.DeleteEvent{Type: eventlog.TypeDelete, StorageID: t.ID, Path: virtualPath, IsDir: false, TS: time.Now().Unix()}); err != nil {
		log.Error().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Err(err).Msg("failed to append eventlog")
		return false, syscall.EIO
	}
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Msg("unlink appended deferred event")
	log.Debug().Str("op", "unlink").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", true).Msg("unlink")
	return true, 0
}

// Rmdir removes a child directory from every read target that has it.
func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n == nil {
		return toErrno(&errkind.NilError{What: "node"})
	}
	rt, log := n.runtime()
	if rt == nil {
		return toErrno(&errkind.NilError{What: "router"})
	}

	caller, callerOK := gofuse.FromContext(ctx)
	if !callerOK {
		return syscall.EPERM
	}

	parentVirtualPath := n.Path(n.Root())
	if parentVirtualPath == "." {
		parentVirtualPath = ""
	}
	virtualPath, errno := joinVirtualPath(parentVirtualPath, name)
	if errno != 0 {
		return errno
	}

	targets, err := rt.ResolveReadTargets(virtualPath)
	if err != nil {
		return toErrno(err)
	}
	log.Debug().Str("op", "rmdir").Str("path", virtualPath).Msg("rmdir resolved targets")

	removed := false

	for _, t := range targets {
		log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", t.Indexed).Msg("rmdir scanning target")
		if !t.Indexed {
			physicalPath := filepath.Join(t.Root, virtualPath)
			st := syscall.Stat_t{}
			if err := syscall.Lstat(physicalPath, &st); err != nil {
				if errors.Is(err, syscall.ENOENT) {
					log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Str("real_path", physicalPath).Msg("rmdir missed on non-indexed target")
					continue
				}
				return toErrno(err)
			}
			if uint32(st.Mode)&syscall.S_IFMT != syscall.S_IFDIR {
				log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Str("real_path", physicalPath).Msg("rmdir rejected non-directory on non-indexed target")
				if removed {
					continue
				}
				return syscall.ENOTDIR
			}
			if callerOK {
				parentPhysical := filepath.Dir(physicalPath)
				pst := syscall.Stat_t{}
				if err := syscall.Lstat(parentPhysical, &pst); err != nil {
					return toErrno(err)
				}
				if uint32(pst.Mode)&syscall.S_IFMT != syscall.S_IFDIR {
					return syscall.ENOTDIR
				}
				if errno := dirWriteExecPermErrno(caller, uint32(pst.Mode), pst.Uid, pst.Gid); errno != 0 {
					return errno
				}
				if errno := stickyDirMayRemoveErrno(caller, uint32(pst.Mode), pst.Uid, st.Uid); errno != 0 {
					return errno
				}
			}
			if errno := toErrno(syscall.Rmdir(physicalPath)); errno != 0 {
				log.Error().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Err(errno).Msg("failed to rmdir")
				return errno
			}
			log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", false).Msg("rmdir")
			removed = true
			continue
		}

		if n.db == nil {
			log.Error().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("failed to rmdir: db is nil for indexed target")
			return syscall.EIO
		}
		f, ok, err := n.db.GetEffectiveFile(ctx, t.ID, virtualPath)
		if err != nil {
			return toErrno(fmt.Errorf("failed to lookup indexed dir: %w", err))
		}
		if !ok {
			log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("rmdir missed on indexed target")
			continue
		}
		if !f.IsDir {
			log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("rmdir rejected non-directory on indexed target")
			if removed {
				continue
			}
			return syscall.ENOTDIR
		}
		if callerOK && parentVirtualPath != "" {
			pdir, ok, err := n.db.GetEffectiveFile(ctx, t.ID, parentVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed parent dir: %w", err))
			}
			if !ok {
				return syscall.ENOENT
			}
			if !pdir.IsDir {
				return syscall.ENOTDIR
			}
			if errno := dirWriteExecPermErrno(caller, pdir.Mode, pdir.UID, pdir.GID); errno != 0 {
				return errno
			}
			if errno := stickyDirMayRemoveErrno(caller, pdir.Mode, pdir.UID, f.UID); errno != 0 {
				return errno
			}
		}
		updated, err := n.db.MarkDeleted(ctx, t.ID, virtualPath, true)
		if err != nil {
			return toErrno(err)
		}
		if !updated {
			log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("rmdir indexed metadata update missed")
			continue
		}
		log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("rmdir updated indexed metadata")
		if err := eventlog.Append(ctx, n.mountName, eventlog.DeleteEvent{Type: eventlog.TypeDelete, StorageID: t.ID, Path: virtualPath, IsDir: true, TS: time.Now().Unix()}); err != nil {
			log.Error().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Err(err).Msg("failed to append eventlog")
			return syscall.EIO
		}
		log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Msg("rmdir appended deferred event")
		log.Debug().Str("op", "rmdir").Str("path", virtualPath).Str("storage_id", t.ID).Bool("indexed", true).Msg("rmdir")
		removed = true
	}

	if removed {
		return 0
	}
	log.Debug().Str("op", "rmdir").Str("path", virtualPath).Msg("rmdir missed on all targets")
	return syscall.ENOENT
}
