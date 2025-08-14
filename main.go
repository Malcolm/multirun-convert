package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

var (
	verbose bool
)

type subprocess struct {
	cmd     *exec.Cmd
	command string
	up      bool
	error   bool
}

func main() {
	flag.BoolVar(&verbose, "v", false, "verbose mode")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <options> command...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	setSubreaper()

	commands := flag.Args()
	if len(commands) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	subprocesses := make(map[int]*subprocess)

	for _, command := range commands {
		cmd := exec.Command("sh", "-c", "exec "+command)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		proc := &subprocess{
			cmd:     cmd,
			command: command,
		}

		err := cmd.Start()
		if err != nil {
			fmt.Fprintf(os.Stderr, "multirun: error starting command '%s': %v\n", command, err)
			continue
		}

		pid := cmd.Process.Pid
		proc.up = true
		subprocesses[pid] = proc

		if verbose {
			fmt.Printf("multirun: launched command \"%s\" with pid %d\n", command, pid)
		}
	}

	exitChan := make(chan exitResult, len(subprocesses))
	for _, proc := range subprocesses {
		go func(p *subprocess) {
			err := p.cmd.Wait()
			exitChan <- exitResult{pid: p.cmd.Process.Pid, err: err}
		}(proc)
	}

	closing := false
	running_processes := len(subprocesses)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for running_processes > 0 {
		select {
		case res := <-exitChan:
			running_processes--
			proc := subprocesses[res.pid]
			proc.up = false

			exitCode := 0
			if res.err != nil {
				if exiterr, ok := res.err.(*exec.ExitError); ok {
					if ws, ok := exiterr.Sys().(syscall.WaitStatus); ok {
						if ws.Signaled() {
							sig := ws.Signal()
							if sig != syscall.SIGINT && sig != syscall.SIGTERM {
								proc.error = true
								if verbose {
									fmt.Printf("multirun: command \"%s\" with pid %d exited abnormally (signal: %s)\n", proc.command, res.pid, sig)
								}
							} else {
								if verbose {
									fmt.Printf("multirun: command \"%s\" with pid %d exited normally (signal: %s)\n", proc.command, res.pid, sig)
								}
							}
						} else {
							exitCode = ws.ExitStatus()
							if exitCode != 0 {
								proc.error = true
								if verbose {
									fmt.Printf("multirun: command \"%s\" with pid %d exited abnormally (exit code: %d)\n", proc.command, res.pid, exitCode)
								}
							}
						}
					}
				} else {
					proc.error = true
					if verbose {
						fmt.Fprintf(os.Stderr, "multirun: command \"%s\" with pid %d failed: %v\n", proc.command, res.pid, res.err)
					}
				}
			}

			if !proc.error && exitCode == 0 {
				if verbose {
					fmt.Printf("multirun: command \"%s\" with pid %d exited normally\n", proc.command, res.pid)
				}
			}

			if !closing {
				closing = true
				if verbose {
					fmt.Println("multirun: one process exited, sending SIGTERM to all other processes")
				}
				killAll(subprocesses, syscall.SIGTERM)
			}

		case sig := <-sigChan:
			if verbose {
				fmt.Printf("multirun: received signal %s, propagating to all subprocesses\n", sig)
			}
			if !closing {
				closing = true
				killAll(subprocesses, sig.(syscall.Signal))
			}
		}
	}

	hadErrors := false
	for _, proc := range subprocesses {
		if proc.error {
			hadErrors = true
			break
		}
	}

	if hadErrors {
		fmt.Fprintln(os.Stderr, "multirun: one or more of the provided commands ended abnormally")
		os.Exit(1)
	}

	if verbose {
		fmt.Println("multirun: all subprocesses exited without errors")
	}
	os.Exit(0)
}

type exitResult struct {
	pid int
	err error
}

func killAll(subprocesses map[int]*subprocess, signal syscall.Signal) {
	for pid, proc := range subprocesses {
		if proc.up {
			err := syscall.Kill(-pid, signal)
			if err != nil {
				if err != syscall.ESRCH {
					fmt.Fprintf(os.Stderr, "multirun: error killing process group %d: %v\n", pid, err)
				}
			}
		}
	}
}
