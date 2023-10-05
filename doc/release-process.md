# DCP Release Process

This repo uses a modified version of the [GitFlow](https://nvie.com/posts/a-successful-git-branching-model/) branching model. The main difference is that we use `main` as our ongoing development branch and an explicit `production` branch for our stable release branch. Feature/development branches should start with `dev/` prefix in order to be properly tracked as a feature branch by [GitVersion](https://gitversion.net/). Official builds are run on [Azure DevOps](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=19235&_a=summary) while PR builds run as GitHub actions.

## Versioning

The project uses [GitVersion](https://gitversion.net/) to generate predictable version numbers. Versions are generated based on the GitVersion.yml configuration and tags; a tagged commit will always generate the same version number. All branches outside of `production` will have a branch specific suffix applied (`-alpha` for `main`, `-rc` for `release/*` branches, and `-pr` for pull request builds). For `main` and `release/*` branches, the prerelease tag will be suffixed with the number of commits since the last tag or upstream version change.

## Release Process

1. Ensure you have the latest branch history by running `git fetch`
1. Create a new release branch from `origin/main` named `release/<version>` where `<version>` is the intended version of the release (`git checkout -b release/<version> origin/main`).
1. Checkout the `release/<version>` branch and run `git merge -s ours origin/production` to ensure the release branch tracks merge history with the `production` branch correctly.
   * If `<version>` is different than the current version reflected by builds from `main` (for example, main builds are at `0.1.27-alpha.10`, but the release is intended to be `0.2.0`) then the new release branch should also be tagged as `v0.2.0-rc.1` at the same time it is first pushed to GitHub. This will ensure that the release branch builds will have the correct version number.
1. Push the new release branch (and tag if applicable) to GitHub.
1. Any further changes during the release process should be made as PRs against the `release` branch.
1. Open a PR to merge the `release` branch into `production`. Once any appropriate validation or bug fixes for the release are complete, the PR should be merged.
1. After the PR is merged, pull the updated `production` locally and tag it with the appropriate version number (eg. `v0.1.27`, `v0.2.0`, etc.) and push the tag to GitHub (`git push --tags`).
1. Create a PR to merge the same `release` branch into `main` and complete without any additional changes to the release branch.

### Potential Process Changes

* We may want to consider creating both PRs from the `release` branch into `production` and `main` at the same time, but it will be important to ensure that the `main` PR isn't completed until after the `production` PR is completed and tagged.

## Release Artifacts

* All builds from `main`, `production`, and `release/*` branches will produce signed build artifacts. In addition, NuGet packages will be uploaded to a feed based on the branch.
   * Builds from `main` publish to the [dcp-development](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp-development/) feed
   * Builds from `release/*` publish to the [dcp-staging](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp-staging) feed
   * Builds from `production` publish to the [dcp](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp) feed
* The `dcp-development` and `dcp-staging` feeds have retention policies set to delete old and unreferenced packages and avoid filling the feeds with unneeded development builds.