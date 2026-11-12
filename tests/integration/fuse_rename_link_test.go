//go:build integration

package integration

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/hieutdo/policyfs/internal/config"
	"github.com/stretchr/testify/require"
)

// TestFUSE_Rename_sameTarget verifies rename works when both old and new path resolve to the same target.
func TestFUSE_Rename_sameTarget(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		RoutingRules: []config.RoutingRule{
			{Match: "fuse-mutate/rename/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
			{Match: "**", WriteTargets: []string{"ssd2"}, ReadTargets: []string{"ssd2"}},
		},
	}, func(env *MountedFS) {
		// Setup: create source file through the mount.
		oldRel := "fuse-mutate/rename/old.txt"
		newRel := "fuse-mutate/rename/new.txt"
		env.MustWriteFileInMountPoint(t, oldRel, []byte("x"))

		// Action: rename through the mount.
		env.MustRenameFileInMountPoint(t, oldRel, newRel)

		// Verify: destination exists on ssd1 only.
		require.NoFileExists(t, env.StoragePath("ssd1", oldRel))
		require.NoFileExists(t, env.StoragePath("ssd2", oldRel))
		require.NoFileExists(t, env.StoragePath("ssd2", newRel))
		b := env.MustReadFileInStoragePath(t, "ssd1", newRel)
		require.Equal(t, []byte("x"), b)
	})
}

// TestFUSE_Rename_crossTarget_returnsEXDEV verifies cross-target rename returns EXDEV.
func TestFUSE_Rename_crossTarget_returnsEXDEV(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		RoutingRules: []config.RoutingRule{
			{Match: "fuse-mutate/rename/src/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
			{Match: "fuse-mutate/rename/dst/**", WriteTargets: []string{"ssd2"}, ReadTargets: []string{"ssd2"}},
			{Match: "**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
		},
	}, func(env *MountedFS) {
		// Setup: cleanup from prior runs and ensure destination parent exists.
		relRoot := "fuse-mutate/rename"
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd1", relRoot)))
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd2", relRoot)))
		env.MustMkdirInMountPoint(t, "fuse-mutate/rename/dst")

		oldRel := "fuse-mutate/rename/src/old.txt"
		newRel := "fuse-mutate/rename/dst/new.txt"
		env.MustWriteFileInMountPoint(t, oldRel, []byte("x"))

		// Action: attempt cross-target rename.
		err := env.RenameFileInMountPoint(oldRel, newRel)
		require.Error(t, err)
		require.ErrorIs(t, err, syscall.EXDEV)

		// Verify: destination not created; source remains.
		require.NoFileExists(t, env.StoragePath("ssd2", newRel))
		require.NoFileExists(t, env.StoragePath("ssd1", newRel))
		b := env.MustReadFileInStoragePath(t, "ssd1", oldRel)
		require.Equal(t, []byte("x"), b)
	})
}

// TestFUSE_Link_sameTarget verifies hardlink works when both paths resolve to the same target.
func TestFUSE_Link_sameTarget(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		RoutingRules: []config.RoutingRule{
			{Match: "fuse-mutate/link/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
			{Match: "**", WriteTargets: []string{"ssd2"}, ReadTargets: []string{"ssd2"}},
		},
	}, func(env *MountedFS) {
		// Setup: cleanup from prior runs.
		relRoot := "fuse-mutate/link"
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd1", relRoot)))
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd2", relRoot)))

		srcRel := "fuse-mutate/link/src.txt"
		dstRel := "fuse-mutate/link/dst.txt"
		env.MustWriteFileInMountPoint(t, srcRel, []byte("x"))

		// Action: create hardlink through the mount.
		require.NoError(t, os.Link(env.MountPath(srcRel), env.MountPath(dstRel)))

		// Verify: both entries exist on ssd1 and share the same inode.
		srcPhysical := env.StoragePath("ssd1", srcRel)
		dstPhysical := env.StoragePath("ssd1", dstRel)
		require.FileExists(t, srcPhysical)
		require.FileExists(t, dstPhysical)
		require.NoFileExists(t, env.StoragePath("ssd2", srcRel))
		require.NoFileExists(t, env.StoragePath("ssd2", dstRel))

		stSrc := env.MustStatT(t, srcPhysical)
		stDst := env.MustStatT(t, dstPhysical)
		require.Equal(t, uint64(stSrc.Ino), uint64(stDst.Ino))
		require.GreaterOrEqual(t, uint64(stSrc.Nlink), uint64(2))
		require.GreaterOrEqual(t, uint64(stDst.Nlink), uint64(2))
	})
}

// TestFUSE_Link_crossTarget_returnsEXDEV verifies cross-target hardlink returns EXDEV.
func TestFUSE_Link_crossTarget_returnsEXDEV(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		RoutingRules: []config.RoutingRule{
			{Match: "fuse-mutate/link/src/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
			{Match: "fuse-mutate/link/dst/**", WriteTargets: []string{"ssd2"}, ReadTargets: []string{"ssd2"}},
			{Match: "**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
		},
	}, func(env *MountedFS) {
		// Setup: cleanup from prior runs and ensure destination parent exists.
		relRoot := "fuse-mutate/link"
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd1", relRoot)))
		require.NoError(t, os.RemoveAll(env.StoragePath("ssd2", relRoot)))
		env.MustMkdirInMountPoint(t, "fuse-mutate/link/dst")

		srcRel := "fuse-mutate/link/src/old.txt"
		dstRel := "fuse-mutate/link/dst/new.txt"
		env.MustWriteFileInMountPoint(t, srcRel, []byte("x"))

		// Action: attempt cross-target hardlink.
		err := os.Link(env.MountPath(srcRel), env.MountPath(dstRel))
		require.Error(t, err)
		require.ErrorIs(t, err, syscall.EXDEV)

		// Verify: destination not created on ssd2.
		require.NoFileExists(t, env.StoragePath("ssd2", dstRel))
	})
}

// TestFUSE_Mv_crossTarget_shouldSucceedViaCopyFallback verifies that `mv` across targets
// (indexed source -> non-indexed destination) succeeds via copy+unlink fallback.
func TestFUSE_Mv_crossTarget_shouldSucceedViaCopyFallback(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		Storages: []IntegrationStorage{
			{ID: "ssd1", Indexed: false, BasePath: "/mnt/ssd1/pfs-integration"},
			{ID: "hdd1", Indexed: true, BasePath: "/mnt/hdd1/pfs-integration"},
		},
		RoutingRules: []config.RoutingRule{
			{Match: "library/music/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1"}},
			{Match: "library/movies/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"hdd1"}},
			{Match: "**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1", "hdd1"}},
		},
	}, func(env *MountedFS) {
		content := []byte("mv-cross-target")
		srcRel := "library/movies/mv-src.txt"
		dstDirRel := "library/music"
		dstRel := dstDirRel + "/mv-src.txt"

		env.MustCreateFileInStoragePath(t, content, "hdd1", srcRel)
		mustRunPFS(t, env, "index", env.MountName)
		require.True(t, env.FileExistsInMountPoint(srcRel))
		env.MustMkdirInMountPoint(t, dstDirRel)

		cmd := exec.Command("mv", env.MountPath(srcRel), env.MountPath(dstDirRel)+"/")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "mv failed: %s", string(out))

		require.False(t, env.FileExistsInMountPoint(srcRel))
		require.True(t, env.FileExistsInMountPoint(dstRel))
		require.Equal(t, content, env.MustReadFileInStoragePath(t, "ssd1", dstRel))
	})
}

// TestFUSE_Rename_renamesAllCopies verifies rename moves the entry on every read target that has it.
func TestFUSE_Rename_renamesAllCopies(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		ReadTargets: []string{"ssd2", "ssd1"},
		Targets:     []string{"ssd2", "ssd1"},
	}, func(env *MountedFS) {
		// Setup: create the same file on both targets.
		oldRel := "fuse-mutate/rename-all/old.txt"
		newRel := "fuse-mutate/rename-all/new.txt"
		env.MustCreateFileInStoragePath(t, []byte("x"), "ssd1", oldRel)
		env.MustCreateFileInStoragePath(t, []byte("x"), "ssd2", oldRel)

		// Action: rename through the mount.
		env.MustRenameFileInMountPoint(t, oldRel, newRel)

		// Verify: renamed on both targets.
		for _, id := range []string{"ssd1", "ssd2"} {
			require.NoFileExists(t, env.StoragePath(id, oldRel))
			require.FileExists(t, env.StoragePath(id, newRel))
		}
	})
}

// TestFUSE_Rename_overwrite_removesStaleDestinationCopies verifies an overwritten destination does not
// survive on targets that held no source copy.
func TestFUSE_Rename_overwrite_removesStaleDestinationCopies(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		ReadTargets: []string{"ssd2", "ssd1"},
		Targets:     []string{"ssd2", "ssd1"},
	}, func(env *MountedFS) {
		// Setup: source on ssd2 only, destination on both targets.
		oldRel := "fuse-mutate/rename-overwrite/old.txt"
		newRel := "fuse-mutate/rename-overwrite/new.txt"
		env.MustCreateFileInStoragePath(t, []byte("new"), "ssd2", oldRel)
		env.MustCreateFileInStoragePath(t, []byte("stale"), "ssd1", newRel)
		env.MustCreateFileInStoragePath(t, []byte("stale"), "ssd2", newRel)

		// Action: rename over the existing destination.
		env.MustRenameFileInMountPoint(t, oldRel, newRel)

		// Verify: ssd2 holds the renamed file and the stale ssd1 copy is gone.
		require.Equal(t, []byte("new"), env.MustReadFileInStoragePath(t, "ssd2", newRel))
		require.NoFileExists(t, env.StoragePath("ssd1", newRel))
	})
}

// TestFUSE_Rename_copyCrossTarget_returnsEXDEVWithoutChanges verifies no copy is renamed when any copy
// would cross targets.
func TestFUSE_Rename_copyCrossTarget_returnsEXDEVWithoutChanges(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		RoutingRules: []config.RoutingRule{
			{Match: "fuse-mutate/rename-exdev/dst/**", WriteTargets: []string{"ssd1"}, ReadTargets: []string{"ssd1", "ssd2"}},
			{Match: "**", WriteTargets: []string{"ssd1", "ssd2"}, ReadTargets: []string{"ssd1", "ssd2"}},
		},
	}, func(env *MountedFS) {
		// Setup: source on both targets; destination is only writable on ssd1.
		oldRel := "fuse-mutate/rename-exdev/src/old.txt"
		newRel := "fuse-mutate/rename-exdev/dst/new.txt"
		env.MustCreateFileInStoragePath(t, []byte("x"), "ssd1", oldRel)
		env.MustCreateFileInStoragePath(t, []byte("x"), "ssd2", oldRel)
		env.MustMkdirInMountPoint(t, "fuse-mutate/rename-exdev/dst")

		// Action: attempt rename.
		err := env.RenameFileInMountPoint(oldRel, newRel)
		require.ErrorIs(t, err, syscall.EXDEV)

		// Verify: neither copy moved.
		for _, id := range []string{"ssd1", "ssd2"} {
			require.FileExists(t, env.StoragePath(id, oldRel))
			require.NoFileExists(t, env.StoragePath(id, newRel))
		}
	})
}
