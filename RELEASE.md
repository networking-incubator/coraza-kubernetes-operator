# Releases

This documentation will help you build and publish a new release of the Coraza
Kubernetes Operator (CKO).

> **Note**: All releases target tags, and our tags follow [semver].

> **Note**: Most of the release process is automated via [GitHub Workflow]. See
> the [release.yml] workflow for details.

[semver]:https://github.com/semver/semver
[GitHub Workflow]:https://docs.github.com/en/actions/concepts/workflows-and-actions/workflows
[release.yml]:https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/.github/workflows/release.yml

## Process

### Step 1 - Communication

Confirm with all other maintainers the plans to cut a release.

This should generally coincide with the completion of one of our [milestones]
for any major or minor releases.

> **Note**: Patch releases may be cut at any time out of `main` or another
> branch depending on the criticality of the patches included.

[milestones]:https://github.com/networking-incubator/coraza-kubernetes-operator/milestones

### Step 2 - Tag

Create a tag off the top of the `main` branch, e.g.:

```console
git tag v0.1.1
```

Push the tag to the repository, e.g.:

```console
git push upstream v0.1.1
```

This will trigger workflows to test and create the release:

- `build-test`
- `release`

You can follow along on the [actions page].

The `release` workflow will:

1. build and tag a container image (e.g. `ghcr.io/networking-incubator/coraza-kubernetes-operator:v0.1.1`)
2. push the tag to GHCR
3. cut a `draft` release from the tag

Stop here and verify that CI has been successful on `main` where the release
was tagged.

> **Note**: tags that start with `v0` or have suffixes including `rc`, `alpha`,
> or `beta` (e.g. `v0.1.1`, `v1.0.0-rc1`, `v0.1.0-alpha1`) will be
> automatically marked as _pre-releases_.

[actions page]:https://github.com/networking-incubator/coraza-kubernetes-operator/actions

### Step 3 - Validating The Release

Once you've confirmed the CI workflows have succeeded for this tag, review the
`draft` release for your tag on the [releases page]. Verify the following are
correct:

- The release **name** should just be the tag name
- Add the major themes and most important changes to the top of the **description**
- The remainder of the **description** should include the auto-generated release notes
- The **crds.yaml**, **operator.yaml**, **samples.yaml** & Helm chart **.tgz** artifacts are attached
  - Check each manifest and the chart package, and verify their correctness
- Make sure the **previous release** is set correctly
  - e.g. for a `v1.0.0` release, _don't_ target `rc`, patch or other pre-releases. Target the last major/minor.

Once you've verified the release, we need to tag the container image appropriately _before_ we publish.

[releases page]:https://github.com/networking-incubator/coraza-kubernetes-operator/releases

### Step 4 - Container Image Tagging

For stable releases (non-prerelease) tag the image as `latest` manually:

```console
# Pull the image
docker pull ghcr.io/networking-incubator/coraza-kubernetes-operator:v0.1.1

# For stable (non-pre-release) versions, also tag as latest
docker tag ghcr.io/networking-incubator/coraza-kubernetes-operator:v0.1.1 ghcr.io/networking-incubator/coraza-kubernetes-operator:latest

# Push the final tags
docker push ghcr.io/networking-incubator/coraza-kubernetes-operator:latest
```

> **Warning**: Do **not** push the `latest` until you are confident the release
> is correct. Pre-release versions (`v0.x.x`, `-alpha`, `-beta`, `-rc`) should
> **not** be tagged as `latest`.

### Step 5 - Publishing & Announcement

> **Warning**: We enforce [immutable releases] so be _absolutely certain_ the
> tests are passing and the release is ready before you publish it.

Publish the release. When publishing, the release page will ask you if you want
to create a discussion to announce the release. Say "Yes" to that and publish an
`announcement` type discussion for the release that links to the release page, or
go to the discussions page and write up an announcement from there.

Make sure the latest release announcement is pinned, and older release
announcements get unpinned.

[immutable releases]:https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases
