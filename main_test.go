//go:build linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
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
	testBin := os.Args[0]

	start := time.Now()
	// The second command will exit immediately with an error code
	cmd := exec.Command(testBin, "-v", "sleep 5", `sh -c "exit 1"`)
	cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

	output, err := cmd.CombinedOutput()
	duration := time.Since(start)

	if testing.Verbose() {
		t.Logf("multirun output:\n%s", string(output))
	}

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
}

func TestSignalPropagation(t *testing.T) {
	testBin := os.Args[0]

	cmd := exec.Command(testBin, "-v", "sleep 5", "sleep 5")
	cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

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

	if testing.Verbose() {
		t.Logf("multirun output:\n%s", output.String())
	}

	// We expect a nil error because multirun should catch the signal and exit gracefully with code 0.
	if waitErr != nil {
		t.Errorf("Expected a nil error after graceful shutdown, but got: %v", waitErr)
	}
}

func TestChainedCommandsAreRejected(t *testing.T) {
	testBin := os.Args[0]

	testCases := []struct {
		name string
		args []string
	}{
		{
			name: "Single command with &&",
			args: []string{"echo hello && echo world"},
		},
		{
			name: "Single command with ;",
			args: []string{"echo hello; echo world"},
		},
		{
			name: "Single command with |",
			args: []string{"echo hello | grep hello"},
		},
		{
			name: "Single command with &",
			args: []string{"sleep 1 &"},
		},
		{
			name: "Multiple commands with one chained",
			args: []string{"echo hello", "sleep 1 && sleep 1"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(testBin, tc.args...)
			cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

			output, err := cmd.CombinedOutput()

			if testing.Verbose() {
				t.Logf("multirun output:\n%s", string(output))
			}

			if err == nil {
				t.Fatal("Expected multirun to exit with an error, but it succeeded.")
			}

			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("Expected an ExitError, but got %T: %v", err, err)
			}

			if exitErr.ExitCode() != 2 {
				t.Errorf("Expected exit code 2, but got %d", exitErr.ExitCode())
			}

			expectedError := "multirun: error: chained commands are not supported."
			if !strings.Contains(string(output), expectedError) {
				t.Errorf("Expected output to contain '%s', but it didn't.\nOutput:\n%s", expectedError, string(output))
			}
		})
	}
}

func TestCommandsWithSpecialCharsInArgsAreAccepted(t *testing.T) {
	testBin := os.Args[0]

	// These commands contain special characters, but they are inside
	// quoted arguments or escaped, so they should be accepted.
	testCases := []struct {
		name string
		args []string
	}{
		{
			name: "Ampersand in double quotes",
			args: []string{`echo "hello&world"`},
		},
		{
			name: "Pipe in single quotes",
			args: []string{`echo 'hello|world'`},
		},
		{
			name: "Semicolon in double quotes",
			args: []string{`echo "hello;world"`},
		},
		{
			name: "Nested quotes",
			args: []string{`echo "a'b&c'd"`},
		},
		{
			name: "Escaped quote",
			args: []string{`echo "a\"b&c\"d"`},
		},
		{
			name: "Escaped backslash",
			args: []string{`echo "a\\&b"`},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(testBin, tc.args...)
			cmd.Env = append(os.Environ(), "GO_TEST_MODE_RUN_MAIN=1")

			output, err := cmd.CombinedOutput()

			if testing.Verbose() {
				t.Logf("multirun output:\n%s", string(output))
			}

			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					if exitErr.ExitCode() == 2 {
						t.Errorf("Expected command to be accepted, but it was rejected with exit code 2.")
					}
				} else {
					t.Fatalf("Command failed with an unexpected error: %v", err)
				}
			}
		})
	}
}
