//go:build !windows

package media

// defaultRecordDir is the fallback [media] record_dir when the key is unset.
// On Unix it follows the FHS state location, matching the systemd unit's
// StateDirectory=/var/lib/eneverre.
var defaultRecordDir = "/var/lib/eneverre/recordings"
