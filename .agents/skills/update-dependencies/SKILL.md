---
name: update-dependencies
description: 'Update packages that DCP executables depend on. This skill is used infrequently, usually just once at the start of a new development cycle. Should only be used when requested explicitly, never as part of a regular change or PR.'
---

# Update Dependencies

Update Go package dependencies stored in @go.mod file.

## When to Use

- Dependency update at the beginning of a new development cycle, to ensure DCP is using the latest versions of packages.
- Upgrading tilt-apiserver to a new release.
- Should only be used when requested explicitly, never as part of a regular change or PR.

## Procedure

### 1. Update the Tilt dependency

Update `tilt-apiserver` dependency to the latest release. Find the latest release version at https://github.com/tilt-dev/tilt-apiserver/releases.

### 2. Update packages shared with Tilt

Fetch the `go.mod` from the tilt-apiserver release tag (e.g. `https://raw.githubusercontent.com/tilt-dev/tilt-apiserver/v<VERSION>/go.mod`). Any *Kubernetes-related* packages (those that have names starting with `k8s.io` or `sigs.k8s.io`) that both `tilt-apiserver` and DCP list in their `go.mod` files should be kept at exactly the same version. This includes both direct and indirect dependencies.

Other (non-Kubernetes) packages that are shared between `tilt-apiserver` and DCP do not need to be kept at the same version. Upgrade them like any other packages (see next step).

### 3. Update other packages

Check the latest versions of other direct dependencies (those **not** marked `// indirect`). Use the Go module proxy to check latest versions: `https://proxy.golang.org/<module>/@latest`.

- If the latest version differs only by **minor or patch** level, update it without asking.
- If the newest version involves a **major version change**, ask the user what to do (one question per package).

Use `go mod edit -require=<module>@<version>` to apply version changes (does not require network access from the terminal).

### 4. Regenerate code

Run `make generate` after updating dependencies. This is especially important after k8s.io package upgrades, as OpenAPI definitions may need regeneration.

## Verification and final steps

### 1. Tidy modules

Run `go mod tidy` and ensure it completes without errors.

### 2. Lint

Run `make lint` and ensure there are no errors or warnings. If you encounter errors or warnings, fix them in the source code.

### 3. Test

Run `make test` and ensure all tests pass. Fix any errors you encounter.

### 4. Update the NOTICE file

Run `make generate-licenses` to update the NOTICE file with the new dependencies. 
