//go:build windows

package media

import (
	"os"
	"path/filepath"
)

// defaultRecordDir is the fallback [media] record_dir when the key is unset.
// On Windows the Unix /var/lib path would land at C:\var\lib\... on the current
// drive, so default to %ProgramData%\Eneverre\recordings instead.
var defaultRecordDir = filepath.Join(programData(), "Eneverre", "recordings")

// programData returns the %ProgramData% root (C:\ProgramData by default).
func programData() string {
	if v := os.Getenv("ProgramData"); v != "" {
		return v
	}
	return `C:\ProgramData`
}
