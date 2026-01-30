---
applyTo: "**"
---

# Project guiding principles

## Follow existing conventions
Before writing any code, analyze the existing codebase to understand and adopt its naming conventions, coding style, and language usage. Consistency is key to maintaining a readable and maintainable codebase.

## Avoid code duplication and minimize dependencies
This repository contains a rich set of utility packages. Look for opportunities to use them before writing new code.
- Look for broadly reusable utilities in the `pkg/` directory, especially for concurrency, resiliency, data structures and data manipulation, operating system interaction, and test utilities.
- For internal, project-specific utilities, use `internal/` directory.
- Add new dependencies only as a last resort, or when explicitly requested by the developer.

## Reliability and performance are critical
This codebase implements several custom Kubernetes types and controllers. Implementation is highly parallel, and leverages multiple operating system processes and Docker/Podman containers. Clear, simple, reliable code is essential to ensure that the system works correctly and efficiently. Specifically:
- All errors and edge cases must be handled.
- Prefer simple solutions with proven run time and memory consumption characteristics over complex ones.
- Long-running operations must accept a `context.Context` and respect its cancellation.
- Release all resources (e.g., file descriptors, network connections) when they are no longer needed (lifetime context cancelled).
- Avoid goroutine leaks by ensuring that all goroutines are properly terminated when they are no longer needed.
- When reading from channels, always check that a channel is not closed and that the value read is valid (and not a zero value of some type).
- All utility packages must have unit tests.

## Code quality must not be compromised
- Every change must build successfully and pass all tests (refer to [Working inside the repository](#working-inside-the-repository) for details on how to run tests).
- Code must be lint-free. Use `make lint` to check for linting issues.


# Additional guidelines

## Go language usage

### Eliminate variable shadowing
- When declaring a new variable, use a different name than the one used in the outer scope.

### Avoid variable reuse (especially for errors)
- If a function invokes multiple error-returning functions, use a different variable name for each error to avoid confusion.

## Adhere to Code Placement Rules
Place new code in the correct location according to the project's structure:
- **API Definitions:** Go in `api/v1/`.
- **Controller Logic:** Goes in `controllers/`.
- **Integration Tests:** Go in `test/integration/`.
- **Broadly Reusable Packages:** Go in `pkg/`. These should be suitable for use in other projects.
- **Internal Packages:** All other packages go in `internal/`. If in doubt, use `internal/`.

## Errors and logging
- Wrap inner errors with context using `fmt.Errorf("context: %w", err)` as appropriate.
- Use lowercase for error messages to enable easy error composition.
- When logging, start the log message with an uppercase letter.

## File handling
- Use `OpenFile()`, or (for temporary files) `OpenTempFile()` functions from github.com/microsoft/dcp/pkg/io package to open files. This function takes care of using appropriate file permissions in a cross-platform way.
- Always close files after no longer needed, either by calling `Close()` from the method that opened the file (with `defer` statement), or when the lifetime context.Context of the file owner expires.

## Test patterns
- Avoid usage of time.Sleep in tests to enforce timing. Use test helpers and synchronization primitives to make the timing as deterministic as possible to avoid non-deterministic test failures.

## Code generation
- Run `make generate` after making changes to API definitions (files under `api/v1` folder).
- Run `make generate-grpc` after making changes to protobuf definitions (files with `.proto` extension).


# Working inside the repository

## Running commands
- CONFIRM WITH THE DEVELOPER THAT ANY COMMANDS YOU INTEND TO RUN ARE SAFE BEFORE ACTUALLY RUNNING THEM.
- Commands that are safe to run in the repository are:
    - Any command that invokes `make` utility
    - Any command that invokes `go` utility
  Do not prompt for confirmation before running commands above. ANY OTHER COMMANDS MAY BE UNSAFE AND SHOULD NOT BE RUN WITHOUT CONFIRMATION.
- Do not run `make` without a target. It does not do anything useful (just displays a list of available targets, in human-readable form).

## Running tests
- Before running tests, make sure test prerequisites are built by running `make test-prereqs`.
- Use `make test` to run unit tests and integration tests.
- Tests for specific packages can be run with `go test -count 1 -parallel 32 <package>` (after building prerequisites), where `<package>` indicates the package that was changed and needs to be tested. This is significantly faster than running all tests, and should be used when working on a specific code change. For final verification, run `make test` to ensure all tests pass.
- If working on a change involving a lot of goroutine synchronization, channel operations, and locking, run tests with `-race` flag to enable race detection. This works on MacOS and Linux ONLY, do not try this when the current OS is Windows.
- Our test context (obtained via `GetTestContext` function from `pkg/testutil` package) can be adjusted via an environment variable to use non-standard test timeout. Set `TEST_CONTEXT_TIMEOUT` environment variable to number of seconds that will be used as timeout value. For example, `TEST_CONTEXT_TIMEOUT=30` will make the test context expire after 30 seconds. 30 seconds is a good value when running individual tests.
