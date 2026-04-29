//go:build !windows

package daemon

import (
	"errors"
	"syscall"
)

// isEMFILE reports whether err is "too many open files" on the calling
// process — the typical accept(2) failure mode when the daemon has
// exhausted its file-descriptor cap.
func isEMFILE(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE)
}

// FDLimit is the soft and hard RLIMIT_NOFILE caps after RaiseFDLimit ran.
type FDLimit struct {
	Soft uint64
	Hard uint64
}

// RaiseFDLimit raises the soft RLIMIT_NOFILE to the hard cap, falling
// back to a high finite ceiling when the OS rejects RLIM_INFINITY. The
// daemon's file watcher holds one descriptor per watched directory on
// Linux (inotify) and additionally one per file inside each watched
// directory on macOS (kqueue), so the inherited soft cap — 1024 on
// most Linuxes, 256 on macOS — is far below what a multi-repo
// installation needs. Callers should log the resulting cap so users
// can correlate "too many open files" against the observed ceiling.
func RaiseFDLimit() (FDLimit, error) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return FDLimit{}, err
	}
	if lim.Cur >= lim.Max {
		return FDLimit{Soft: uint64(lim.Cur), Hard: uint64(lim.Max)}, nil
	}
	want := lim
	want.Cur = lim.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &want); err == nil {
		return FDLimit{Soft: uint64(want.Cur), Hard: uint64(want.Max)}, nil
	}
	// macOS rejects Cur=RLIM_INFINITY even when Max claims it. Try
	// progressively lower finite ceilings so a single Setrlimit failure
	// doesn't leave the daemon stuck on the inherited soft cap.
	for _, ceiling := range []uint64{1 << 20, 1 << 16, 1 << 14, 1 << 13} {
		if uint64(lim.Cur) >= ceiling {
			continue
		}
		want.Cur = ceiling
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &want); err == nil {
			return FDLimit{Soft: uint64(want.Cur), Hard: uint64(want.Max)}, nil
		}
	}
	return FDLimit{Soft: uint64(lim.Cur), Hard: uint64(lim.Max)}, nil
}
