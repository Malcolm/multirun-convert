//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// setSubreaper ensures that multirun adopts any orphaned grandchild processes.
// This is crucial for cleaning up all descendants properly.
func setSubreaper() {
	// From linux/prctl.h, since this is not exported by the standard syscall package.
	const PR_SET_CHILD_SUBREAPER = 36
	// We make a raw syscall to avoid depending on golang.org/x/sys
	// and to keep the project self-contained.
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, PR_SET_CHILD_SUBREAPER, 1, 0)
	if errno != 0 {
		// This is not a fatal error, but we should log it if verbose.
		// We can't use the logger here because it's not initialized yet.
		if os.Getenv("MULTIRUN_VERBOSE") == "1" {
			fmt.Printf("multirun: failed to register as subreaper (errno: %d), subchildren exit status might be ignored.\n", errno)
		}
	}
}

// subprocess holds the state of a single child process.
type subprocess struct {
	cmd     *exec.Cmd
	command string
	up      bool
	err     error
}

// multirun holds the application's state and configuration.
type multirun struct {
	verbose      bool
	subprocesses map[int]*subprocess
	exitChan     chan *subprocess
	sigChan      chan os.Signal
}

func main() {
	// We need to set the subreaper status as early as possible.
	setSubreaper()

	app := &multirun{
		subprocesses: make(map[int]*subprocess),
		exitChan:     make(chan *subprocess, 1),
		sigChan:      make(chan os.Signal, 1),
	}

	flag.BoolVar(&app.verbose, "v", false, "verbose mode")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <options> command...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// The verbose flag is used by the logger, so we set it as an env var
	// for the subreaper function, which runs before the flag is parsed.
	if app.verbose {
		os.Setenv("MULTIRUN_VERBOSE", "1")
	}

	commands := flag.Args()
	if len(commands) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if err := app.startSubprocesses(commands); err != nil {
		fmt.Fprintf(os.Stderr, "multirun: %v\n", err)
		os.Exit(2) // Exit with a different code for startup errors
	}

	// If no processes were started, exit immediately.
	if len(app.subprocesses) == 0 {
		if app.verbose {
			fmt.Println("multirun: no processes were successfully started.")
		}
		os.Exit(1)
	}

	hadErrors := app.handleEvents()

	if hadErrors {
		fmt.Fprintln(os.Stderr, "multirun: one or more of the provided commands ended abnormally")
		os.Exit(1)
	}

	if app.verbose {
		fmt.Println("multirun: all subprocesses exited without errors")
	}
	os.Exit(0)
}

// startSubprocesses launches all the commands as child processes.
func (app *multirun) startSubprocesses(commands []string) error {
	for _, command := range commands {
		if isChained(command) {
			return fmt.Errorf("error: chained commands are not supported. Please provide each command as a separate argument")
		}

		cmd := exec.Command("sh", "-c", "exec "+command)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		proc := &subprocess{
			cmd:     cmd,
			command: command,
		}

		if err := cmd.Start(); err != nil {
			// Log the error but continue trying to start other processes.
			fmt.Fprintf(os.Stderr, "multirun: error starting command '%s': %v\n", command, err)
			continue
		}

		pid := cmd.Process.Pid
		proc.up = true
		app.subprocesses[pid] = proc
		app.logf("launched command \"%s\" with pid %d", command, pid)

		// Start a goroutine to wait for this process to exit.
		go func(p *subprocess) {
			p.err = p.cmd.Wait()
			app.exitChan <- p
		}(proc)
	}
	return nil
}

// handleEvents is the main event loop. It waits for signals or process exits
// and returns true if any process exited with an error.
func (app *multirun) handleEvents() (hadErrors bool) {
	signal.Notify(app.sigChan, syscall.SIGINT, syscall.SIGTERM)

	runningProcesses := len(app.subprocesses)
	closing := false

	for runningProcesses > 0 {
		select {
		case proc := <-app.exitChan:
			runningProcesses--
			proc.up = false

			if !isNormalExit(proc.err) {
			// Overwrite the error with a simple one for tracking.
			proc.err = fmt.Errorf("abnormal exit")
				app.logf("command \"%s\" with pid %d exited abnormally", proc.command, proc.cmd.Process.Pid)
			} else {
			// Clear the error if the exit was normal (e.g., exit 0 or killed by SIGTERM).
			proc.err = nil
				app.logf("command \"%s\" with pid %d exited normally", proc.command, proc.cmd.Process.Pid)
			}

			if !closing {
				closing = true
				app.logf("one process exited, sending SIGTERM to all other processes")
				app.shutdown(syscall.SIGTERM)
			}

		case sig := <-app.sigChan:
			if !closing {
				closing = true
				app.logf("received signal %s, propagating to all subprocesses", sig)
				app.shutdown(sig.(syscall.Signal))
			}
		}
	}

	// After all processes have exited, check if any of them had an error.
	for _, proc := range app.subprocesses {
		if proc.err != nil {
			return true
		}
	}
	return false
}

// shutdown sends the given signal to all running subprocesses.
func (app *multirun) shutdown(signal syscall.Signal) {
	for pid, proc := range app.subprocesses {
		if proc.up {
			// Kill the entire process group.
			if err := syscall.Kill(-pid, signal); err != nil {
				// ESRCH means the process is already dead, which is fine.
				if err != syscall.ESRCH {
					fmt.Fprintf(os.Stderr, "multirun: error killing process group %d: %v\n", pid, err)
				}
			}
		}
	}
}

// isNormalExit checks if a process exit error is considered "normal".
// Normal exits are exit code 0 or termination by SIGINT/SIGTERM.
func isNormalExit(err error) bool {
	if err == nil {
		return true // Exit code 0
	}

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		// This should not happen with cmd.Wait()
		return false
	}

	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}

	if ws.Exited() {
		return ws.ExitStatus() == 0
	}

	if ws.Signaled() {
		sig := ws.Signal()
		return sig == syscall.SIGINT || sig == syscall.SIGTERM
	}

	return false
}

// isChained checks if a command string contains unquoted shell operators.
// It handles single quotes, double quotes, and backslash escapes.
func isChained(command string) bool {
	var inQuote rune = 0
	var escaped bool = false
	for _, r := range command {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if inQuote != 0 {
			if r == inQuote {
				inQuote = 0 // End of quote
			}
		} else {
			switch r {
			case '\'', '"':
				inQuote = r // Start of quote
			case ';', '|', '&':
				return true // Found an unquoted operator
			}
		}
	}
	return false
}

// logf prints a formatted message to stdout if verbose mode is enabled.
func (app *multirun) logf(format string, v ...interface{}) {
	if app.verbose {
		fmt.Printf("multirun: "+format+"\n", v...)
	}
}
