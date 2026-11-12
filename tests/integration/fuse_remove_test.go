//go:build integration

package integration

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFUSE_Unlink_removesAllCopies verifies UNLINK deletes the file from every read target that has it.
func TestFUSE_Unlink_removesAllCopies(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		ReadTargets: []string{"ssd2", "ssd1"},
		Targets:     []string{"ssd2", "ssd1"},
	}, func(env *MountedFS) {
		// Setup: create the same file on both targets.
		rel := "fuse-mutate/unlink/x.txt"
		env.MustCreateFileInStoragePath(t, []byte("from ssd1"), "ssd1", rel)
		env.MustCreateFileInStoragePath(t, []byte("from ssd2"), "ssd2", rel)

		// Action: unlink through the mount.
		env.MustRemoveFileInMountPoint(t, rel)

		// Verify: removed from both targets and no longer visible.
		require.NoFileExists(t, env.StoragePath("ssd2", rel))
		require.NoFileExists(t, env.StoragePath("ssd1", rel))
		require.False(t, env.FileExistsInMountPoint(rel))
	})
}

// TestFUSE_Rmdir_removesAllCopies verifies RMDIR deletes the directory from every read target that has it.
func TestFUSE_Rmdir_removesAllCopies(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		ReadTargets: []string{"ssd2", "ssd1"},
		Targets:     []string{"ssd2", "ssd1"},
	}, func(env *MountedFS) {
		// Setup: create the same directory on both targets.
		rel := "fuse-mutate/rmdir/d"
		env.MustCreateDirInStoragePath(t, "ssd1", rel)
		env.MustCreateDirInStoragePath(t, "ssd2", rel)

		// Action: rmdir through the mount.
		env.MustRemoveFileInMountPoint(t, rel)

		// Verify: removed from both targets.
		require.NoDirExists(t, env.StoragePath("ssd2", rel))
		require.NoDirExists(t, env.StoragePath("ssd1", rel))
	})
}

// TestFUSE_Rmdir_nonEmptyCopy_shouldReturnENOTEMPTY verifies RMDIR fails when any copy still has entries.
func TestFUSE_Rmdir_nonEmptyCopy_shouldReturnENOTEMPTY(t *testing.T) {
	withMountedFS(t, IntegrationConfig{
		ReadTargets: []string{"ssd2", "ssd1"},
		Targets:     []string{"ssd2", "ssd1"},
	}, func(env *MountedFS) {
		// Setup: empty directory on ssd2, same directory with a file on ssd1.
		rel := "fuse-mutate/rmdir-notempty/d"
		env.MustCreateDirInStoragePath(t, "ssd2", rel)
		env.MustCreateFileInStoragePath(t, []byte("x"), "ssd1", rel+"/f.txt")

		// Action: rmdir through the mount.
		err := env.RemoveFileInMountPoint(rel)

		// Verify: ENOTEMPTY and the file is still visible.
		require.ErrorIs(t, err, syscall.ENOTEMPTY)
		require.True(t, env.FileExistsInMountPoint(rel+"/f.txt"))
	})
}
