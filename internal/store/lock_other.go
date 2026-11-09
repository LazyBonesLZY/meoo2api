//go:build !unix

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// Non-Unix platforms use an exclusive lockfile. An interrupted process may
// leave it behind; remove it only after confirming that server has stopped.
func lockDirectory(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("数据目录已锁定，请先停止服务并检查 .lock: %w", err)
	}
	return f, nil
}
func unlockDirectory(f *os.File) error {
	name := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}
func syncDirectory(f *os.File) error { return nil }
