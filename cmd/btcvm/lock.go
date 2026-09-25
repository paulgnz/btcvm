package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// errBridgeRunning means another process holds the bridge lock.
var errBridgeRunning = errors.New("the bridge is running; stop it first")

// lockBridge takes the lock that lets one process at a time move peg funds
// with the signer set at signersPath: the bridge while it runs, or a
// refund. Without it a refund could race the running bridge and both
// credit and refund one deposit. The lock is released when the process
// exits.
func lockBridge(signersPath string) (*os.File, error) {
	path := filepath.Join(filepath.Dir(signersPath), "bridge.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errBridgeRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}
