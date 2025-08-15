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

// logf prints a formatted message to stdout if verbose mode is enabled.
func logf(verbose bool, format string, v ...interface{}) {
	if verbose {
		fmt.Printf("multirun: "+format+"\n", v...)
	}
}

// setSubreaper ensures that multirun adopts any orphaned grandchild processes.
func setSubreaper(verbose bool) {
	// From linux/prctl.h, since this is not exported by the standard syscall package.
	const PR_SET_CHILD_SUBREAPER = 36
	// We make a raw syscall to avoid depending on golang.org/x/sys
	// and to keep the project self-contained.
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, PR_SET_CHILD_SUBREAPER, 1, 0)
	if errno != 0 {
		logf(verbose, "failed to register as subreaper (errno: %d), subchildren exit status might be ignored.", errno)
	} else {
		logf(verbose, "successfully registered as subreaper.")
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
	// 1. Define and parse command-line flags immediately.
	var verbose bool
	flag.BoolVar(&verbose, "v", false, "verbose mode")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <options> command...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// 2. Set subreaper status, now that we know the verbose setting.
	setSubreaper(verbose)

	// 3. Create the application instance.
	app := &multirun{
		verbose:      verbose,
		subprocesses: make(map[int]*subprocess),
		exitChan:     make(chan *subprocess, 1),
		sigChan:      make(chan os.Signal, 1),
	}

	commands := flag.Args()
	if len(commands) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	if err := app.startSubprocesses(commands); err != nil {
		fmt.Fprintf(os.Stderr, "multirun: %v\n", err)
		os.Exit(2)
	}

	if len(app.subprocesses) == 0 {
		logf(app.verbose, "no processes were successfully started.")
		os.Exit(1)
	}

	hadErrors := app.handleEvents()

	if hadErrors {
		fmt.Fprintln(os.Stderr, "multirun: one or more of the provided commands ended abnormally")
		os.Exit(1)
	}

	logf(app.verbose, "all subprocesses exited without errors")
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
			fmt.Fprintf(os.Stderr, "multirun: error starting command '%s': %v\n", command, err)
			continue
		}

		pid := cmd.Process.Pid
		proc.up = true
		app.subprocesses[pid] = proc
		logf(app.verbose, "launched command \"%s\" with pid %d", command, pid)

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
				proc.err = fmt.Errorf("abnormal exit")
				logf(app.verbose, "command \"%s\" with pid %d exited abnormally", proc.command, proc.cmd.Process.Pid)
			} else {
				proc.err = nil
				logf(app.verbose, "command \"%s\" with pid %d exited normally", proc.command, proc.cmd.Process.Pid)
			}

			if !closing {
				closing = true
				logf(app.verbose, "one process exited, sending SIGTERM to all other processes")
				app.shutdown(syscall.SIGTERM)
			}

		case sig := <-app.sigChan:
			if !closing {
				closing = true
				logf(app.verbose, "received signal %s, propagating to all subprocesses", sig)
				app.shutdown(sig.(syscall.Signal))
			}
		}
	}

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
			if err := syscall.Kill(-pid, signal); err != nil {
				if err != syscall.ESRCH {
					fmt.Fprintf(os.Stderr, "multirun: error killing process group %d: %v\n", pid, err)
				}
			}
		}
	}
}

// isNormalExit checks if a process exit error is considered "normal".
func isNormalExit(err error) bool {
	if err == nil {
		return true
	}

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
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
				inQuote = 0
			}
		} else {
			switch r {
			case '\'', '"':
				inQuote = r
			case ';', '|', '&':
				return true
			}
		}
	}
	return false
}
