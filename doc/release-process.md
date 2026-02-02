# DCP Release Process

Feature/development branches should start with a `dev/` prefix and merged into main via PR. Release candidates (and final releases) are managed through versioned release branches. A given major.minor version of DCP will have a corresponding release branch following the pattern `release/<major>.<minor>`. All official releases, including servicing releases will be produced from these release branches. When a release is ready to ship, a new GitHub release should be created to tag the commit in the appropriate release branch with the format  `v<major>.<minor>.<patch>` (eg. v0.2.3) and a new build of the official build pipeline should be run for that tag. Servicing changes should be made to main if appropriate and cherry-picked as a PR to the appropriate release branch(es). If the change only needs to be made in release branches, it can be made in the most recent release and cherry-picked to other applicable release branches from there.

## Versioning

The project uses [GitVersion](https://gitversion.net/) to generate predictable version numbers. Versions are generated based on the GitVersion.yml configuration and tags; a tagged commit will always generate the same version number. All branches have a branch specific suffix applied by default (`-alpha` for `main`, `-rc` for `release/*` branches, and `-pr` for pull request builds). For `main` and `release/*` branches, the prerelease tag will be suffixed with the number of commits since the last tag or upstream version change. An RC commit should be promoted to a full release by tagging with the appropriate `v<major>.<minor>.<patch>` tag.

## Release Process

### Starting a new major or minor release

* Note: pushing a new release branch requires JIT elevated permissions.

1. Ensure you have the latest branch history by running `git fetch`
1. Create a new release branch from `origin/main` named `release/<major>.<minor>` where `<major>.<minor>` is the intended version of the release (`git checkout -b release/<major>.<minor> origin/main`).
1. Push the new release branch to GitHub by running `git push origin`

*Note: A given release branch will be used for all releases, including servicing releases in the given major.minor version. So `release/0.2` will serve as the release branch for any `0.2.x` releases, `release/0.3` would be for `0.3.x`, etc. Tags will be used to indicate the specific commits within a release branch that are final release builds. (See [Creating a final release build](#starting-a-final-release-build) for more details on tagging and building the final release build).*

### Making dual check-in changes to a release branch

1. If a change needs to be made to both the release branch and main, the change can be made first in the main branch and then, once the PR is merged, a backport PR can be created to merge the changes into a target release branch by commenting on the original PR in the format `/backport to <target branch>` (for example, `/backport to release/0.2`). This will kick off a GitHub action to create a backport branch.
1. Currently, automatic creation of the PR will fail, but if the bot is able to create the new backport branch (there are no merge conflicts with the target release branch), it will add a comment to the PR with a link that will take you to the "Create a PR" page with all fields pre-filled. Fill out the template and submit the new PR.
1. If for some reason creating the new branch fails, you'll have to resort to manually cherry-picking the PR changes into a new branch (created from the release branch) and handling any merge conflicts yourself, before creating the new PR manually.

### Starting a final release build

* Note: creating a new tagged GitHub release requires JIT elevated permissions.

1. Once you have a release candidate build that you're ready to release as a final build, [start a new GitHub release](https://github.com/microsoft/dcp/releases/new).
1. Set the release tag to the appropriate `v<major>.<minor>.<patch>` (eg. `v0.2.3`) and the target branch to either the release branch you're releasing from (eg. `release/0.2`) if the latest commit in the branch is the new release build or to the specific commit from the release branch.
1. Either leave the previous tag as auto or manually select the previous release tag and click `Generate release notes` (if the generated changelog doesn't look correct and you used automatic tag detection, you may have to try creating the release again and manually selecting the previous release).
1. Ensure `Set as a pre-release` is unchecked and `Set as the latest release` is checked.
1. Click `Publish release`.
1. Go to the official build pipeline and verify that a build is running for the newly created tag. This should be automatically triggered. If a build doesn't automatically start after a short time, you can manually start a new build for the tag you just created (`refs/tags/v<major>.<minor>.<patch>`).
1. Once this build completes, download the binary .zip/.tar.gz files from the `build_archive` artifact and upload each individual binary archive to the GitHub release you just created (click the edit icon in the top right corner of the release and drag the files into the drop zone at the bottom of the edit box).
1. Click `Update release` to save the changes.


### Updating the DCP build in Aspire

Once the production branch is updated and tagged, an official build needs to be started for that tagged version:

1. Tags aren't currently automatically picked up by the signing pipeline, so you'll need to ensure the new release tag is synced to the signing pipeline.
1.  Once the new tag has been synced, an official build should start automatically but you can trigger one manually for the tag if necessary.
1. Keep an eye on the official build pipeline in case there are any issues. It should kick off an automatic PR in Aspire to pull in the new bits.
1.  Watch for a PR titled "Update dependencies microsoft/dcp", which updates
[eng/Versions.props](https://github.com/dotnet/aspire/blob/main/eng/Versions.props) and [eng/Version.Details.xml](https://github.com/dotnet/aspire/blob/main/eng/Versions.props) files.
    * When the PR shows up, review and approve. Once the automatic checks pass, the PR should auto-merge.
1.  The final check is to verify the correct version of DCP is included in the Aspire nugets. Follow the [Aspire daily build usage instructions](https://github.com/dotnet/aspire/blob/main/docs/using-latest-daily.md) to get the updated build on your machine, and then run `dcp.exe version` from the Aspire hosting orchestration Nuget package. Verify that the version reported is the version that was published.

    * The path to DCP binary should be something similar to `<user home>\.nuget\packages\aspire.hosting.orchestration.win-x64\<version>\tools` (replace the name of the package as necessary depending on your platform).
    * You might have different versions of this package, so make sure you do the verification using the latest one.

