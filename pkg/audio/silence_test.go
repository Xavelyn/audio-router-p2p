package audio

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"
)

func TestSuppressStderr(t *testing.T) {
	// Verify normal stderr write works before suppression
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pipeR.Close()

	// Redirect os.Stderr to our pipe
	oldStderr := os.Stderr
	os.Stderr = pipeW
	defer func() {
		os.Stderr = oldStderr
	}()

	// Re-entrant call test
	suppressStderr(func() {
		// Inside suppression: writing to stderr fd (fd 2) should go to /dev/null
		_, _ = os.Stderr.Write([]byte("suppressed outer\n"))

		suppressStderr(func() {
			_, _ = os.Stderr.Write([]byte("suppressed inner\n"))
		})

		_, _ = os.Stderr.Write([]byte("suppressed outer after inner\n"))
	})

	// After suppression completes, writes should appear in pipeW
	_, _ = os.Stderr.Write([]byte("visible after suppression\n"))
	_ = pipeW.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, pipeR)
	output := buf.String()

	if output != "visible after suppression\n" {
		t.Errorf("expected only 'visible after suppression\\n', got %q", output)
	}
}

func TestSuppressStderrConcurrency(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			suppressStderr(func() {
				_, _ = os.Stderr.Write([]byte("test concurrent\n"))
				val, err := withSuppressedStderr(func() (int, error) {
					return id, nil
				})
				if err != nil || val != id {
					t.Errorf("unexpected val: %d, err: %v", val, err)
				}
			})
		}(i)
	}
	wg.Wait()
}
