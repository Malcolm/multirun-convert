# Multirun Agent Instructions

## About this Project

This project, `multirun`, is a simple Go utility for running multiple commands concurrently. It's designed to be a lightweight process manager, particularly for containerized environments.

The key behaviors are:
- It runs multiple commands, each in its own process group.
- It propagates signals (SIGINT, SIGTERM) to all child processes.
- If any child process terminates, it terminates all other child processes.
- It waits for all children to exit before exiting itself.
- It exits with a non-zero status code if any child exits with an error.

## Development and Testing

The project is contained entirely within `main.go` and has no external dependencies. This is an intentional design choice to keep the utility simple and portable.

### Building

To build the executable, run:
```bash
go build .
```
This will produce a `multirun` binary in the root directory.

### Testing

The tests are located in `main_test.go`. To run the tests, use the standard Go test command:
```bash
go test -v
```

The tests use a clever technique: the test binary itself is re-executed with a special environment variable (`GO_TEST_MODE_RUN_MAIN=1`) to act as the `multirun` program being tested. This avoids the need for a pre-compiled binary.

## Key Directives

1.  **Maintain Simplicity:** The core value of this project is its simplicity. Avoid adding new features, external dependencies, or complex logic unless absolutely necessary.
2.  **Single Source File:** All core logic should remain within the `main.go` file.
3.  **Linux Only:** The project is intended for Linux environments, as indicated by the `//go:build linux` directive. Ensure any changes are compatible with this constraint.
4.  **Test Thoroughly:** Any changes to the logic must be accompanied by corresponding tests. Ensure all existing tests continue to pass.
5.  **Respect the Original's Behavior:** This is a Go port of a C utility. The fundamental behavior described in the `README.md` should be preserved.
