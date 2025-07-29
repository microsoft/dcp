---
applyTo: "**"
---

# Project guiding principles

## Follow existing conventions
Before writing any code, analyze the existing codebase to understand and adopt its naming conventions, coding style, and language usage. Consistency is key to maintaining a readable and maintainable codebase.

## Avoid Code Duplication
This repository contains a rich set of utility packages. Reuse them as much as possible instead of reinventing the wheel.
- Look for broadly reusable utilities in the `pkg/` directory, especially for concurrency, resiliency, data structures and data manipulation, operating system interaction, and test utilities.
- For internal, project-specific utilities, use `internal/` directory.

## Reliability and performance are critical
This codebase implements several custom Kubernetes types and controllers. Implementation is highly parallel, and leverages multiple operating system processes and Docker/Podman containers. Clear, simple, reliable code is essential to ensure that the system works correctly and efficiently. Specifically:
- All errors and edge cases must be handled.
- Prefer simple solutions with proven run time and memory consumption characteristics over complex ones.
- Long-running operations must accept a `context.Context` and respect its cancellation.
- Release all resources (e.g., file descriptors, network connections) when they are no longer needed (lifetime context cancelled).
- Avoid goroutine leaks by ensuring that all goroutines are properly terminated when they are no longer needed.
- When reading from channels, always ensure that 
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
- **Internal Packages:** All other packages go in `internal/`. If in doubt, use `internal/`.**

## Errors and logging
- Wrap inner errors with context using `fmt.Errorf("context: %w", err)` as appropriate.
- Use lowercase for error messages to enable easy error composition.
- When logging, start the log message with an uppercase letter.


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
- Tests for specific packages can be run with `go test -count 1 -parallel 32 <package>` (after building prerequisites).
- If working on a change involving a lot of parallel operations, run tests with `-race` flag to enable race detection. This works on MacOS and Linux ONLY, do not try this when running on Windows.
