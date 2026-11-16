package mover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hieutdo/policyfs/internal/config"
	"github.com/hieutdo/policyfs/internal/errkind"
	"github.com/hieutdo/policyfs/internal/indexdb"
)

// mirrorCandidate copies c to every destination that lacks an up-to-date copy.
// The source is deleted only once every destination is in sync.
// It returns true when the per-file deadline was reached and the job must stop.
func (p *planner) mirrorCandidate(ctx context.Context, j config.MoverJobConfig, hooks Hooks, db *indexdb.DB, dstIDs []string, c candidate, deadline time.Time, jr *JobResult) (bool, error) {
	warn := func(dstID string, err error) {
		if hooks.Warn != nil {
			hooks.Warn(j.Name, c.SrcStorageID, c.RelPath, fmt.Errorf("mirror to %s: %w", dstID, err))
		}
	}

	srcRoot := p.storageByID[c.SrcStorageID].Path
	srcLocation := copyLocation{root: srcRoot, physicalPath: filepath.Join(srcRoot, c.RelPath)}

	srcIsDest := false
	partial := false
	changed := false
	for _, dstID := range dstIDs {
		if dstID == c.SrcStorageID {
			srcIsDest = true
			continue
		}
		state, err := p.mirrorDestState(ctx, db, dstID, c)
		if err != nil {
			partial = true
			warn(dstID, err)
			continue
		}
		if state == mirrorInSync {
			continue
		}
		if state == mirrorNewer {
			// The copy was likely edited on the destination; keep it and the source until someone reconciles them.
			partial = true
			warn(dstID, errMirrorDestinationNewer)
			continue
		}
		if len(p.filterAvailableDestinations([]string{dstID})) == 0 {
			partial = true
			warn(dstID, errNoDestinationAvailable)
			continue
		}

		if hooks.FileStart != nil {
			hooks.FileStart(j.Name, c.SrcStorageID, dstID, c.RelPath, c.SizeBytes)
		}
		if !p.opts.DryRun {
			if err := p.mirrorCopy(ctx, j, hooks, db, srcLocation, dstID, c, deadline); err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return true, nil
				}
				if cerr := ctx.Err(); cerr != nil {
					return false, fmt.Errorf("move canceled: %w", cerr)
				}
				if _, serr := os.Stat(srcLocation.physicalPath); errors.Is(serr, os.ErrNotExist) {
					jr.FilesSkipped++
					if hooks.Warn != nil {
						hooks.Warn(j.Name, c.SrcStorageID, c.RelPath, fmt.Errorf("source disappeared: %w", err))
					}
					return false, nil
				}
				partial = true
				warn(dstID, err)
				continue
			}
		}
		changed = true
		jr.BytesMoved += c.SizeBytes
		if hooks.Progress != nil {
			hooks.Progress(j.Name, dstID, c.RelPath)
		}
	}

	if partial {
		jr.FilesPartial++
	} else if !srcIsDest && jobDeleteSourceEnabled(j) {
		if !p.opts.DryRun {
			ok, err := p.deleteSource(ctx, j, hooks, db, c, srcLocation.physicalPath, jr)
			if err != nil || !ok {
				return false, err
			}
		}
		changed = true
	}
	if changed {
		jr.FilesMoved++
	}
	return false, nil
}

// mirrorState describes a destination copy relative to the source candidate.
type mirrorState int

const (
	// mirrorMissing means the destination has no regular file at the path.
	mirrorMissing mirrorState = iota
	// mirrorStale means the destination copy differs and is not newer than the source.
	mirrorStale
	// mirrorInSync means the destination copy has the same size and mtime as the source.
	mirrorInSync
	// mirrorNewer means the destination copy has a newer mtime than the source.
	mirrorNewer
)

// errMirrorDestinationNewer marks a destination copy left alone because it is newer than the source.
var errMirrorDestinationNewer = errkind.SentinelError("destination copy is newer than source")

// classifyMirrorCopy compares a destination copy's attributes with the source candidate.
// A newer destination mtime wins over a size difference so edits made on a destination are never overwritten.
func classifyMirrorCopy(regular bool, size int64, mtimeSec int64, c candidate) mirrorState {
	switch {
	case !regular:
		return mirrorMissing
	case mtimeSec > c.MTimeSec:
		return mirrorNewer
	case size == c.SizeBytes && mtimeSec == c.MTimeSec:
		return mirrorInSync
	default:
		return mirrorStale
	}
}

// mirrorDestState reports how dstID's copy of c compares to the source.
// Indexed destinations are checked through the index so sleeping disks are not woken.
func (p *planner) mirrorDestState(ctx context.Context, db *indexdb.DB, dstID string, c candidate) (mirrorState, error) {
	sp := p.storageByID[dstID]
	if sp.Indexed {
		if db == nil {
			return mirrorMissing, &errkind.NilError{What: "index db"}
		}
		f, ok, err := db.GetEffectiveFile(ctx, dstID, c.RelPath)
		if err != nil {
			return mirrorMissing, fmt.Errorf("failed to lookup indexed destination: %w", err)
		}
		if !ok {
			return mirrorMissing, nil
		}
		return classifyMirrorCopy(!f.IsDir, f.Size, f.MTimeSec, c), nil
	}

	fi, err := os.Lstat(filepath.Join(sp.Path, c.RelPath))
	if errors.Is(err, os.ErrNotExist) {
		return mirrorMissing, nil
	}
	if err != nil {
		return mirrorMissing, fmt.Errorf("failed to stat destination: %w", err)
	}
	return classifyMirrorCopy(fi.Mode().IsRegular(), fi.Size(), fi.ModTime().Unix(), c), nil
}

// mirrorCopy writes c to dstID, replacing any stale copy, and records it in the index when indexed.
func (p *planner) mirrorCopy(ctx context.Context, j config.MoverJobConfig, hooks Hooks, db *indexdb.DB, src copyLocation, dstID string, c candidate, deadline time.Time) error {
	dstSP := p.storageByID[dstID]
	dst := copyLocation{root: dstSP.Path, physicalPath: filepath.Join(dstSP.Path, c.RelPath), overwrite: true}

	copyCtx := ctx
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		copyCtx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	var progress func(phase string, doneBytes int64, totalBytes int64)
	if hooks.CopyProgress != nil {
		progress = func(phase string, doneBytes int64, totalBytes int64) {
			hooks.CopyProgress(j.Name, dstID, c.RelPath, phase, doneBytes, totalBytes)
		}
	}
	if err := copyFileWithVerifyRetry(copyCtx, src, dst, c, jobVerifyEnabled(j), defaultCopyRetries, progress); err != nil {
		return err
	}

	if !dstSP.Indexed {
		return nil
	}
	sz := c.SizeBytes
	if err := db.UpsertFile(ctx, dstID, c.RelPath, false, &sz, c.MTimeSec, c.Mode, c.UID, c.GID); err != nil {
		return fmt.Errorf("failed to index destination: %w", err)
	}
	return nil
}
