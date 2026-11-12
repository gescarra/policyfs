package fuse

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hieutdo/policyfs/internal/errkind"
	"github.com/hieutdo/policyfs/internal/eventlog"
	"github.com/hieutdo/policyfs/internal/router"
	"github.com/rs/zerolog"
)

// renameSource is one existing copy of a rename source.
type renameSource struct {
	target       router.Target
	physicalPath string
	st           syscall.Stat_t
}

// Rename renames every copy of a child, each within its own underlying target.
//
// Cross-target renames return EXDEV.
func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if n == nil {
		return toErrno(&errkind.NilError{What: "node"})
	}
	rt, log := n.runtime()
	if rt == nil {
		return toErrno(&errkind.NilError{What: "router"})
	}
	if flags != 0 {
		// go-fuse uses flags for RENAME_EXCHANGE/RENAME_NOREPLACE; we don't support those yet.
		return syscall.ENOTSUP
	}

	caller, callerOK := gofuse.FromContext(ctx)
	if !callerOK {
		return syscall.EPERM
	}

	np, ok := newParent.(*Node)
	if !ok {
		return syscall.EXDEV
	}
	if np.state != n.state {
		// Different runtime state == different logical filesystem.
		return syscall.EXDEV
	}

	oldParentVirtualPath := n.Path(n.Root())
	if oldParentVirtualPath == "." {
		oldParentVirtualPath = ""
	}
	oldVirtualPath, errno := joinVirtualPath(oldParentVirtualPath, name)
	if errno != 0 {
		return errno
	}

	var srcs []renameSource
	{
		targets, err := rt.ResolveReadTargets(oldVirtualPath)
		if err != nil {
			return toErrno(err)
		}
		log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Msg("rename resolved source targets")
		for _, t := range targets {
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Bool("indexed", t.Indexed).Msg("rename scanning source target")
			if !t.Indexed {
				p := filepath.Join(t.Root, oldVirtualPath)
				st := syscall.Stat_t{}
				if err := syscall.Lstat(p, &st); err != nil {
					if errors.Is(err, syscall.ENOENT) {
						log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Str("real_path", p).Msg("rename missed on non-indexed source target")
						continue
					}
					return toErrno(err)
				}
				srcs = append(srcs, renameSource{target: t, physicalPath: p, st: st})
				continue
			}

			if n.db == nil {
				log.Error().Str("op", "rename").Str("path", oldVirtualPath).Str("storage_id", t.ID).Msg("failed to rename: db is nil for indexed target")
				return syscall.EIO
			}
			_, ok, err := n.db.GetEffectiveFile(ctx, t.ID, oldVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed source: %w", err))
			}
			if ok {
				srcs = append(srcs, renameSource{target: t})
				log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Msg("rename resolved indexed source file")
				continue
			}
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Msg("rename indexed source file missed; probing directory")
			dirOK, err := n.db.DirExists(ctx, t.ID, oldVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed source dir: %w", err))
			}
			if dirOK {
				srcs = append(srcs, renameSource{target: t})
				log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Msg("rename resolved indexed source directory")
				continue
			}
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("storage_id", t.ID).Msg("rename missed on indexed source target")
		}
		if len(srcs) == 0 {
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Msg("rename missed on all source targets")
			return syscall.ENOENT
		}
	}

	newParentVirtualPath := np.Path(np.Root())
	if newParentVirtualPath == "." {
		newParentVirtualPath = ""
	}
	newVirtualPath, errno := joinVirtualPath(newParentVirtualPath, newName)
	if errno != 0 {
		return errno
	}

	// Indexed copies must stay readable at the new path and non-indexed copies writable.
	// Every copy is checked before any is renamed so a cross-target rename changes nothing.
	readable, err := rt.ResolveReadTargets(newVirtualPath)
	if err != nil {
		return toErrno(err)
	}
	allowed, err := rt.ResolveWriteTargets(newVirtualPath)
	if err != nil {
		return toErrno(err)
	}
	for _, src := range srcs {
		domain := allowed
		if src.target.Indexed {
			domain = readable
		}
		if !slices.ContainsFunc(domain, func(t router.Target) bool { return t.ID == src.target.ID }) {
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", src.target.ID).Msg("rename blocked: cross-target")
			return syscall.EXDEV
		}
	}

	for _, src := range srcs {
		if errno := n.renameOnTarget(ctx, log, caller, src, oldParentVirtualPath, oldVirtualPath, newParentVirtualPath, newVirtualPath); errno != 0 {
			return errno
		}
	}

	// Remove overwritten destination copies on targets without a source copy so they cannot resurface.
	for _, t := range readable {
		if slices.ContainsFunc(srcs, func(src renameSource) bool { return src.target.ID == t.ID }) {
			continue
		}
		if _, errno := np.unlinkOnTarget(ctx, log, caller, t, newParentVirtualPath, newVirtualPath); errno != 0 && errno != syscall.EISDIR {
			return errno
		}
	}
	return 0
}

// renameOnTarget renames one copy within its own target.
func (n *Node) renameOnTarget(ctx context.Context, log zerolog.Logger, caller *gofuse.Caller, src renameSource, oldParentVirtualPath string, oldVirtualPath string, newParentVirtualPath string, newVirtualPath string) syscall.Errno {
	srcTarget := src.target

	if srcTarget.Indexed {
		if n.db == nil {
			log.Error().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Msg("failed to rename: db is nil for indexed target")
			return syscall.EIO
		}
		// For indexed targets, rename is a metadata update + deferred physical rename.
		srcEntry, ok, err := n.db.GetEffectiveFile(ctx, srcTarget.ID, oldVirtualPath)
		if err != nil {
			return toErrno(fmt.Errorf("failed to lookup indexed source: %w", err))
		}
		if !ok {
			return syscall.ENOENT
		}
		if oldParentVirtualPath != "" {
			pdir, ok, err := n.db.GetEffectiveFile(ctx, srcTarget.ID, oldParentVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed src parent: %w", err))
			}
			if !ok || !pdir.IsDir {
				return syscall.ENOTDIR
			}
			if errno := dirWriteExecPermErrno(caller, pdir.Mode, pdir.UID, pdir.GID); errno != 0 {
				return errno
			}
			if errno := stickyDirMayRemoveErrno(caller, pdir.Mode, pdir.UID, srcEntry.UID); errno != 0 {
				return errno
			}
		}
		if newParentVirtualPath != "" {
			pdir, ok, err := n.db.GetEffectiveFile(ctx, srcTarget.ID, newParentVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed dst parent: %w", err))
			}
			if !ok || !pdir.IsDir {
				return syscall.ENOTDIR
			}
			if errno := dirWriteExecPermErrno(caller, pdir.Mode, pdir.UID, pdir.GID); errno != 0 {
				return errno
			}
			dstEntry, ok, err := n.db.GetEffectiveFile(ctx, srcTarget.ID, newVirtualPath)
			if err != nil {
				return toErrno(fmt.Errorf("failed to lookup indexed overwrite target: %w", err))
			}
			if ok {
				if errno := stickyDirMayRemoveErrno(caller, pdir.Mode, pdir.UID, dstEntry.UID); errno != 0 {
					return errno
				}
			}
		}

		updated, err := n.db.RenamePath(ctx, srcTarget.ID, oldVirtualPath, newVirtualPath)
		if err != nil {
			return toErrno(err)
		}
		if !updated {
			log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Msg("rename indexed metadata update missed")
			return syscall.ENOENT
		}
		log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Msg("rename updated indexed metadata")
		if err := eventlog.Append(ctx, n.mountName, eventlog.RenameEvent{Type: eventlog.TypeRename, StorageID: srcTarget.ID, OldPath: oldVirtualPath, NewPath: newVirtualPath, TS: time.Now().Unix()}); err != nil {
			log.Error().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Err(err).Msg("failed to append eventlog")
			return syscall.EIO
		}
		log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Msg("rename appended deferred event")
		log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Bool("indexed", true).Msg("rename")
		return 0
	}

	oldParentPhysical := filepath.Dir(src.physicalPath)
	pst := syscall.Stat_t{}
	if err := syscall.Lstat(oldParentPhysical, &pst); err != nil {
		return toErrno(err)
	}
	if uint32(pst.Mode)&syscall.S_IFMT != syscall.S_IFDIR {
		return syscall.ENOTDIR
	}
	if errno := dirWriteExecPermErrno(caller, uint32(pst.Mode), pst.Uid, pst.Gid); errno != 0 {
		return errno
	}
	if errno := stickyDirMayRemoveErrno(caller, uint32(pst.Mode), pst.Uid, src.st.Uid); errno != 0 {
		return errno
	}

	dstPhysicalPath := filepath.Join(srcTarget.Root, newVirtualPath)
	// Ensure destination parent dirs exist on the source target.
	if err := materializeParentDirs(ctx, srcTarget.Root, newVirtualPath); err != nil {
		log.Error().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Err(err).Msg("failed to materialize parent dirs")
		return toErrno(err)
	}
	log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Msg("rename materialized destination parent dirs")
	newParentPhysical := filepath.Dir(dstPhysicalPath)
	if err := syscall.Lstat(newParentPhysical, &pst); err != nil {
		return toErrno(err)
	}
	if uint32(pst.Mode)&syscall.S_IFMT != syscall.S_IFDIR {
		return syscall.ENOTDIR
	}
	if errno := dirWriteExecPermErrno(caller, uint32(pst.Mode), pst.Uid, pst.Gid); errno != 0 {
		return errno
	}
	dstSt := syscall.Stat_t{}
	if err := syscall.Lstat(dstPhysicalPath, &dstSt); err == nil {
		if errno := stickyDirMayRemoveErrno(caller, uint32(pst.Mode), pst.Uid, dstSt.Uid); errno != 0 {
			return errno
		}
	} else if !errors.Is(err, syscall.ENOENT) {
		return toErrno(err)
	}
	renameErrno := toErrno(syscall.Rename(src.physicalPath, dstPhysicalPath))
	if renameErrno != 0 {
		log.Error().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Err(renameErrno).Msg("failed to rename")
	} else {
		log.Debug().Str("op", "rename").Str("old_path", oldVirtualPath).Str("new_path", newVirtualPath).Str("storage_id", srcTarget.ID).Bool("indexed", false).Msg("rename")
	}
	return renameErrno
}
