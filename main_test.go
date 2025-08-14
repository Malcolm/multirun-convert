package main

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"runtime"
)

// TestMain allows us to intercept the test run.
// If the test is re-executed with a specific env var, we run the main() function.
// This allows the test binary itself to act as the program we want to test.
func TestMain(m *testing.M) {
	if os.Getenv("GO_TEST_MODE_RUN_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFailureCase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows due to reliance on 'sleep' and 'sh' commands")
	}
	testBin := os.Args[0]

	start := time.Now()
	// The second command will exit immediately with an error code
	cmd := exec.Command(testBin, "sleep 5", `sh -c "exit 1"`)
	cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	if err == nil {
		t.Fatal("Expected multirun to exit with an error, but it succeeded.")
	}

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("Expected an ExitError, but got %T: %v", err, err)
	}

	if exitErr.ExitCode() != 1 {
		t.Errorf("Expected exit code 1, but got %d", exitErr.ExitCode())
	}

	// It should exit very quickly because the second command fails immediately, killing the first.
	if duration > 1*time.Second {
		t.Errorf("Expected multirun to exit quickly, but it took %v", duration)
	}
	t.Logf("multirun exited quickly as expected. Output:\n%s", string(output))
}

func TestSignalPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping test on Windows due to reliance on POSIX signals")
	}
	testBin := os.Args[0]

	cmd := exec.Command(testBin, "sleep 5", "sleep 5")
	cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

	err := cmd.Start()
	if err != nil {
		t.Fatalf("Failed to start multirun: %v", err)
	}

	// Give it a moment to launch the children
	time.Sleep(200 * time.Millisecond)

	// Send SIGTERM to the multirun process
	err = cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		t.Fatalf("Failed to send SIGTERM to multirun: %v", err)
	}

	// Wait for the process to exit
	waitErr := cmd.Wait()

	// We expect a nil error because multirun should catch the signal and exit gracefully with code 0.
	if waitErr != nil {
		t.Errorf("Expected a nil error after graceful shutdown, but got: %v", waitErr)
	} else {
		t.Logf("multirun exited gracefully as expected.")
	}
}
