//go:build unix

package audio

import (
	"os"
	"sync"
	"syscall"
)

var (
	silenceMu    sync.Mutex
	silenceDepth int
	savedStderr  int
	devNullFile  *os.File
)

// suppressStderr executes fn while temporarily redirecting OS stderr (fd 2) to /dev/null.
// This silences C/C++ library probe spam from ALSA and JACK (e.g. jack server not running,
// unknown PCM phoneline, modem) during PortAudio initialization, device enumeration, and termination.
// Reentrant and concurrent-safe across goroutines.
func suppressStderr(fn func()) {
	silenceMu.Lock()
	if silenceDepth == 0 {
		var err error
		devNullFile, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err == nil {
			stderrFd := int(os.Stderr.Fd())
			savedStderr, err = syscall.Dup(stderrFd)
			if err == nil {
				_ = syscall.Dup2(int(devNullFile.Fd()), stderrFd)
			}
		}
	}
	silenceDepth++
	silenceMu.Unlock()

	defer func() {
		silenceMu.Lock()
		silenceDepth--
		if silenceDepth == 0 {
			if savedStderr != 0 {
				stderrFd := int(os.Stderr.Fd())
				_ = syscall.Dup2(savedStderr, stderrFd)
				_ = syscall.Close(savedStderr)
				savedStderr = 0
			}
			if devNullFile != nil {
				_ = devNullFile.Close()
				devNullFile = nil
			}
		}
		silenceMu.Unlock()
	}()

	fn()
}

func withSuppressedStderr[T any](fn func() (T, error)) (T, error) {
	var val T
	var err error
	suppressStderr(func() {
		val, err = fn()
	})
	return val, err
}

func withSuppressedStderrErr(fn func() error) error {
	var err error
	suppressStderr(func() {
		err = fn()
	})
	return err
}
