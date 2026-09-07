//go:build linux

package host

import "golang.org/x/sys/unix"

// diskUsage returns used-space percent and used-inode percent for the filesystem at path.
func diskUsage(path string) (usedPct int, inodePct int, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	if st.Blocks > 0 {
		usedPct = int((st.Blocks - st.Bavail) * 100 / st.Blocks)
	}
	if st.Files > 0 {
		inodePct = int((st.Files - st.Ffree) * 100 / st.Files)
	}
	return usedPct, inodePct, nil
}
