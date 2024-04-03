# DCP Release Process

Feature/development branches should start with a `dev/` previx and merged into main via PR. Release candidates (and final releases) are managed through versioned release branches. A given major.minor version of DCP will have a corresponding release branch following the pattern `release/<major>.<minor>.0`. All official releases, including servicing releases will be produced from these release branches. When a release is ready to ship, a tag should be applied to the appropriate commit in the relevant release branch with the format v<major>.<minor>.<patch> (eg. v0.2.3) and a new build of the [official AzDO pipeline](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=19235&_a=summary) should be run for that tag. Servicing changes should be made to main if appropriate and cherry-picked as a PR to the appropriate release branch(es). If the change only needs to be made in release branches, it can be made in the most recent release and cherry-picked from there.

We previously used a production branch to manage releases, but this has been replaced by this new release branch strategy. The production branch was used for the 0.1.x releases and is now deprecated. The production branch should not be used for any new releases.

## Versioning

The project uses [GitVersion](https://gitversion.net/) to generate predictable version numbers. Versions are generated based on the GitVersion.yml configuration and tags; a tagged commit will always generate the same version number. All branches have a branch specific suffix applied by default (`-alpha` for `main`, `-rc` for `release/*` branches, and `-pr` for pull request builds). For `main` and `release/*` branches, the prerelease tag will be suffixed with the number of commits since the last tag or upstream version change. An RC commit should be promoted to a full release by tagging with the appropriate `v<major>.<minor>.<patch>` tag.

## Release Process

### Starting a new major or minor release

1. Ensure you have the latest branch history by running `git fetch`
1. Create a new release branch from `origin/main` named `release/<major>.<minor>` where `<major>.<minor>` is the intended version of the release (`git checkout -b release/<major>.<minor> origin/main`).
1. Push the new release branch (and tag if applicable) to GitHub by running `git push origin`
1. Any further changes to finalize the release should be made with dual check-in to both the `main` and `release` branch.
1. Once we're ready to complete the release fetch the latest changes `git fetch`, copy the hash of the appropriate commit in the release branch (usually the latest commit), and tag the commit with the appropriate version number (`git tag v<major>.<minor>.0 <commit hash>`). Finally push the new tag to GitHub (`git push --tags`).
1. After the tag is pushed to GitHub, manually run a new [official build](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=19235&_a=summary) and specify the new tag as the Branch/tag (eg. `refs/tags/v0.2.0`). This build will produce the final release artifacts and publish them to the appropriate feed.

### Starting a new servicing release

1. In order to service a given release, you can simply make PRs directly against the relevant release branch(es). If the changes apply to main and/or other release branches as well, they should be backported via cherry-pick PRs as appropriate. Subsequent commits to the release branch after a release tag will have their patch version incremented (and a `-rc` tag applied, so the commit after v0.2.0 would be v0.2.1-rc.0).
1. Once we're ready to complete the servicing release fetch the latest changes `git fetch`, copy the hash of the appropriate commit in the release branch (usually the latest commit), and tag the commit with the appropriate version number (`git tag v<major>.<minor>.<patch> <commit hash>`). Finally push the new tag to GitHub (`git push --tags`).
1. After the tag is pushed to GitHub, manually run a new [official build](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=19235&_a=summary) and specify the new tag as the Branch/tag (eg. `refs/tags/v0.2.3`). This build will produce the final release artifacts and publish them to the appropriate feed.

### Updating the DCP build in Aspire

Once the production branch is updated and tagged, the Aspire repository should be updated automatically. To verify this happened:

1.  Ensure that [official build](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=19235&_a=summary) for the updated `release` branch tag has completed.
1.  Watch for a PR titled "Update dependencies microsoft/usvc-apiserver", which updates
[eng/Versions.props](https://github.com/dotnet/aspire/blob/main/eng/Versions.props) and [eng/Version.Details.xml](https://github.com/dotnet/aspire/blob/main/eng/Versions.props) files.
    * When the PR shows up, review and approve. Once the automatic checks pass, the PR should auto-merge.
    * If the PR build is not showing up for some reason, you can check the [Build Promotion Pipeline](https://dev.azure.com/devdiv/DevDiv/_build?definitionId=12603&_a=summary) which is dotnet infrastructure that handles the actual publishing of builds to feeds. A run should have been started by our build's deploy stage.
1.  The final check is to verify the correct version of DCP is coming with the workload. Follow the [Aspire daily build usage instructions](https://github.com/dotnet/aspire/blob/main/docs/using-latest-daily.md#install-the-net-aspire-dotnet-workload) to get the workload updated on your machine, and then run `dcp.exe version` from the Aspire hosting orchestration pack. Verify that the version reported is the version that was published.

    * The path to DCP binary should be something similar to `\Program Files\dotnet\packs\Aspire.Hosting.Orchestration.win-x64\8.0.0-preview.2.23517.10\tools`
    * You might have different versions of this pack, so make sure you do the verification using the latest one.


## Release Artifacts

* All builds from `main`, and `release/*` branches will produce signed build artifacts. In addition, NuGet packages will be uploaded to a feed based on the branch.
   * Builds from `main` publish to the [dcp-development](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp-development/) feed
   * RC builds from `release/*` publish to the [dcp-staging](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp-staging) feed
   * Final tagged builds from `release/*` publish to the [dcp](https://dev.azure.com/devdiv/DevDiv/_artifacts/feed/dcp) feed
* The `dcp-development` and `dcp-staging` feeds have retention policies set to delete old and unreferenced packages and avoid filling the feeds with unneeded development builds.