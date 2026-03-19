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

The release workflow pushes the operator image tagged as the git tag
(e.g. `v0.1.1`). The OLM bundle references the image by its immutable
digest, so the tag does not affect OLM installs.

For stable (non-pre-release) versions, tag the image as `latest` before
publishing the release:

```console
docker pull ghcr.io/networking-incubator/coraza-kubernetes-operator:v0.1.1
docker tag ghcr.io/networking-incubator/coraza-kubernetes-operator:v0.1.1 ghcr.io/networking-incubator/coraza-kubernetes-operator:latest
docker push ghcr.io/networking-incubator/coraza-kubernetes-operator:latest
```

> **Warning**: Do **not** push the `latest` tag until you are confident the
> release is correct. Pre-release versions (`v0.x.x`, `-alpha`, `-beta`,
> `-rc`) should **not** be tagged as `latest`.

### OLM (Operator Lifecycle Manager)

The release workflow builds and pushes three container images. Two of them
are specific to OLM:

```
  Operator image (:v0.2.0)
       │
       │  generate_bundle.py (make bundle)
       │  Renders Helm chart, extracts Deployment/RBAC/CRDs,
       │  injects into CSV template (image pinned by digest)
       ▼
  Bundle image (:v0.2.0)
  ┌─────────────────────┐
  │ manifests/          │  CSV, CRDs, Service, etc.
  │ metadata/           │  OLM annotations
  │ tests/scorecard/    │  scorecard config
  └─────────────────────┘
       │
       │  opm render (inside catalog docker build)
       │  Pulls bundle image, extracts full content
       ▼
  Catalog image (:v0.2.0)
  ┌─────────────────────┐
  │ catalog.yaml        │  olm.package + olm.channel
  │ + rendered bundles  │  full CSV/CRD content from opm render
  └─────────────────────┘
       │
       │  deployed as CatalogSource CR
       ▼
  OLM / OperatorHub UI
```

**What the release workflow does automatically:**

1. `make bundle` — generates bundle manifests from the Helm chart
2. `make bundle.build bundle.push` — builds and pushes the bundle image
3. `make catalog.update` — adds the new version to `catalog.yaml` channel entries
   (with `replaces` pointing to the previous version)
4. `make catalog.build catalog.push` — builds the catalog image (runs `opm render`
   inside the Docker build to pull each bundle image and embed its full content)

**Key files:**

| File | Role |
|---|---|
| `bundle/base/csv-template.yaml` | CSV template — edit to change OLM metadata |
| `hack/generate_bundle.py` | Generates bundle from Helm chart + CSV template |
| `catalog/coraza-kubernetes-operator/catalog.yaml` | Package and channel definitions (no bundle content) |
| `hack/update_catalog.py` | Adds new versions to catalog channel entries |
| `catalog/Dockerfile` | Multi-stage build: uses `opm render` to embed bundle content |

**What needs manual attention:**

- **`catalog.yaml` must be committed before tagging the release.** The release
  workflow runs `catalog.update` in its working tree but does **not** commit the
  result back to the repository. If you skip this step, the next release will
  start from a stale checkout and the OLM upgrade chain will break — each
  release's catalog will have no `replaces` link to the previous version,
  making every version an independent install instead of an upgrade.

  Before tagging, run:

  ```console
  make catalog.update VERSION=vX.Y.Z
  git add catalog/coraza-kubernetes-operator/catalog.yaml
  git commit -m "catalog: add vX.Y.Z to OLM channel"
  ```

  Automating this commit-back step is tracked in
  https://github.com/networking-incubator/coraza-kubernetes-operator/issues/184.

- If the CSV template needs changes (description, icon, install modes, etc.),
  edit `bundle/base/csv-template.yaml` before the release.

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
