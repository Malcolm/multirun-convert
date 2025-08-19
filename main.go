//go:build linux

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
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

// commandWithPrefix stores a command and its associated output prefix.
type commandWithPrefix struct {
	prefix  string
	command string
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
	wg           sync.WaitGroup
}

func main() {
	var verbose bool
	var configFile string
	flag.BoolVar(&verbose, "v", false, "verbose mode")
	flag.StringVar(&configFile, "f", "", "path to config file with commands")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <options> [command...]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	setSubreaper(verbose)

	commands, err := loadCommands(configFile, flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "multirun: %v\n", err)
		os.Exit(2)
	}

	if len(commands) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	app := &multirun{
		verbose:      verbose,
		subprocesses: make(map[int]*subprocess),
		exitChan:     make(chan *subprocess, len(commands)),
		sigChan:      make(chan os.Signal, 1),
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

	app.wg.Wait()

	if hadErrors {
		fmt.Fprintln(os.Stderr, "multirun: one or more of the provided commands ended abnormally")
		os.Exit(1)
	}

	logf(app.verbose, "all subprocesses exited without errors")
	os.Exit(0)
}

// loadCommands loads commands from a config file or command line arguments.
func loadCommands(configFile string, args []string) ([]commandWithPrefix, error) {
	if configFile != "" {
		return loadCommandsFromFile(configFile)
	}

	if len(args) == 0 {
		return nil, nil
	}

	var commands []commandWithPrefix
	for _, arg := range args {
		commands = append(commands, commandWithPrefix{command: arg, prefix: ""}) // prefix is empty for now
	}
	return commands, nil
}

// loadCommandsFromFile reads commands from a file.
func loadCommandsFromFile(path string) ([]commandWithPrefix, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error opening config file: %w", err)
	}
	defer file.Close()

	var commands []commandWithPrefix
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		var cmd commandWithPrefix
		if len(parts) == 2 {
			cmd.prefix = strings.TrimSpace(parts[0])
			cmd.command = strings.TrimSpace(parts[1])
		} else {
			cmd.command = line
			cmd.prefix = line // Default prefix is the command itself
		}
		commands = append(commands, cmd)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	return commands, nil
}

// startSubprocesses launches all the commands as child processes.
func (app *multirun) startSubprocesses(commands []commandWithPrefix) error {
	for _, c := range commands {
		if isChained(c.command) {
			return fmt.Errorf("error: chained commands are not supported. Please provide each command as a separate argument")
		}

		cmd := exec.Command("sh", "-c", "exec "+c.command)
		cmd.Env = os.Environ()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		// If a prefix is defined, set up pipes to capture and prefix output.
		// Otherwise, connect directly to stdout/stderr.
		if c.prefix != "" {
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				return fmt.Errorf("error creating stdout pipe for '%s': %w", c.command, err)
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				return fmt.Errorf("error creating stderr pipe for '%s': %w", c.command, err)
			}
			app.wg.Add(2)
			go app.prefixOutput(stdout, c.prefix, os.Stdout)
			go app.prefixOutput(stderr, c.prefix, os.Stderr)
		} else {
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
		}

		proc := &subprocess{
			cmd:     cmd,
			command: c.command,
		}

		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "multirun: error starting command '%s': %v\n", c.command, err)
			continue
		}

		pid := cmd.Process.Pid
		proc.up = true
		app.subprocesses[pid] = proc
		logf(app.verbose, "launched command \"%s\" with pid %d", c.command, pid)

		go func(p *subprocess) {
			p.err = p.cmd.Wait()
			app.exitChan <- p
		}(proc)
	}
	return nil
}

// prefixOutput reads from a reader, prefixes each line, and writes to a writer.
func (app *multirun) prefixOutput(reader io.Reader, prefix string, writer io.Writer) {
	defer app.wg.Done()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fmt.Fprintf(writer, "[%s] %s\n", prefix, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		logf(app.verbose, "error reading output for prefix '%s': %v", prefix, err)
	}
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
