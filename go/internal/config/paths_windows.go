//go:build windows

package config

import (
	"os"
	"path/filepath"
)

// On Windows the Unix FHS locations (/etc, /var) don't exist, so override the
// default search paths with the conventional per-machine data root
// %ProgramData%\Eneverre (e.g. C:\ProgramData\Eneverre). The portable ".\data"
// fallback stays second, so running eneverre.exe from a folder that contains a
// data\ directory still works. init runs after the package-level var
// initializers, so these replace the Unix defaults on Windows only.
func init() {
	root := filepath.Join(programData(), "Eneverre")
	configPaths = []string{filepath.Join(root, "eneverre.ini"), filepath.Join("data", "eneverre.ini")}
	camerasDirs = []string{filepath.Join(root, "cameras.d"), filepath.Join("data", "cameras.d")}
	dbPaths = []string{filepath.Join(root, "eneverre.db"), filepath.Join("data", "eneverre.db")}
}

// programData returns the %ProgramData% root (C:\ProgramData by default).
func programData() string {
	if v := os.Getenv("ProgramData"); v != "" {
		return v
	}
	return `C:\ProgramData`
}
