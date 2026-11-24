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
| `bundle/base/ci.yaml` | OperatorHub PR metadata (`reviewers`, `updateGraph`); copied to `bundle/ci.yaml` by `make bundle` |
| `hack/generate_bundle.py` | Generates bundle from Helm chart + CSV template |
| `hack/publish_operatorhub.sh` | Opens the PR against `k8s-operatorhub/community-operators` |
| `hack/operatorhub-pr-template.md` | Body of that PR (the upstream review checklist) |
| `hack/operatorhub_opp_test.sh` | Runs the upstream OPP test tiers against a staged bundle |
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

**Platform flavours.** `make bundle` renders the chart for vanilla Kubernetes,
which is what OperatorHub.io requires. `make bundle BUNDLE_PLATFORM=openshift`
renders the OpenShift flavour (no explicit `runAsUser`/`fsGroup`, so the SCC
assigns the UID range, and `istio.revision=openshift-gateway`) for the separate
[`redhat-openshift-ecosystem/community-operators-prod`] submission. Both are
validated on every PR by the `bundle validate` job.

**Serving certificate.** Both flavours render with `tls.generate=false`, so the
bundle contains no Secret and no private key. The operator's install contract
is unchanged from Helm: it expects the xDS serving Secret to exist already. For
a Helm install the chart creates it; for a bundle install the cluster
administrator creates it before subscribing. That prerequisite is spelled out
in the CSV description and in the OperatorHub install guide, and it is load
bearing: the pod does not start until the Secret exists, so OLM reports the
install as failed if it is missing.

Never render the bundle with `tls.generate=true`. The generate branch mints a
private key, and the bundle is published to a public Git repository and
catalog image.

[`redhat-openshift-ecosystem/community-operators-prod`]:https://github.com/redhat-openshift-ecosystem/community-operators-prod

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

### Step 6 - Post-Release Updates & Github Pages

After the release is published and the container image is available, open a
follow-up PR to `main` with the following changes:

**Bump Chart version** — update `charts/coraza-kubernetes-operator/Chart.yaml`:

```yaml
version: 0.5.0        # chart version (without 'v' prefix)
appVersion: "0.5.0"   # operator version (quoted string, without 'v' prefix)
```

While the release workflow overrides these via `helm package --version` and
`--app-version`, the values in the repository must be correct for users who
install from a local checkout (e.g. `helm install ./charts/...`). The
`appVersion` also serves as the default `image.tag` when `image.tag` is not
set in values — bumping it before the image exists would break local installs.

**Update docs versioning** — update `docs/versions.yaml` to add the new release
version and set it as the default. This must also happen **after** tagging
because `hack/build-versioned-docs.sh` verifies that every non-HEAD ref exists
and will fail if the tag is missing.

```yaml
versions:
  - name: latest
    ref: v0.5.0
    display: v0.5 (latest)
    default: true
  - name: v0.4
    ref: v0.4.0
    display: v0.4
    default: false
  - name: dev
    ref: HEAD
    display: dev
    default: false
```

Once the PR merges, re-publish the Helm chart index and docs by using one of
these options:
1. Trigger the [github Pages](https://github.com/networking-incubator/coraza-kubernetes-operator/actions/workflows/pages.yml) workflow manually from the GitHub UI.
2. Run the CLI command `gh workflow run pages.yml`.

### Step 7 - OperatorHub.io

Publishing to [OperatorHub.io] means opening a pull request against
[`k8s-operatorhub/community-operators`] that adds
`operators/coraza-kubernetes-operator/<X.Y.Z>/{manifests,metadata}` plus the
shared `operators/coraza-kubernetes-operator/ci.yaml`. Everything in that tree
is produced by `make bundle`; nothing is hand-written in the upstream repo.

Two rules from upstream shape the whole process:

- The bundle must work on **vanilla Kubernetes**. That is why `make bundle`
  defaults to `BUNDLE_PLATFORM=kubernetes`. The OpenShift flavour goes to
  [`redhat-openshift-ecosystem/community-operators-prod`] as a *separate* PR;
  an operator published to OperatorHub.io does not appear in OpenShift's
  embedded catalogue and vice versa.
- Every commit must be **signed off** (`git commit -s`, real name, `Author`
  and `Signed-off-by` matching). `publish_operatorhub.sh` passes `-s` and uses
  `GIT_USER` / `GIT_EMAIL` for both.

The [`operatorhub.yml`] workflow that would do all of this on release is
**disabled** (`if: false` on the job) until a dedicated bot or service account
can hold the PAT and related settings for pushing to a fork and opening PRs.
Re-enabling it is tracked in [issue #201](https://github.com/networking-incubator/coraza-kubernetes-operator/issues/201).

Until then, submit the bundle **locally** after the GitHub release exists and
the operator image is published to GHCR.

1. Check out the **release tag** (same commit you released).

2. Generate the bundle with the **same** controller image reference as the
   release (bare tag in `VERSION`, full image ref for the env):

   ```console
   export CONTROLLER_MANAGER_CONTAINER_IMAGE=ghcr.io/networking-incubator/coraza-kubernetes-operator:vX.Y.Z
   make bundle VERSION=vX.Y.Z
   ```

3. Validate the bundle against the upstream suites:

   ```console
   make bundle.validate
   ```

   This runs `operator-sdk bundle validate` with `suite=operatorframework`
   (which covers `operatorhubv2`, `capabilities`, `categories`,
   `alpha-deprecated-apis`, `good-practices` and `multiarch`). It must report
   *All validation tests have completed successfully*. One warning is expected
   and benign: the deprecated-APIs validator flags the `events` RBAC rule
   because `events.k8s.io/v1beta1` was removed in Kubernetes 1.25. The rule is
   version-agnostic and `events.k8s.io/v1` is current.

4. (Recommended) Run the upstream OPP test the CI pipeline will run:

   ```console
   ./hack/operatorhub_opp_test.sh --version X.Y.Z
   ```

   Use the **bare** semver (`X.Y.Z`, no `v`) for `--version`. It needs docker,
   ansible and jmespath, and it stages the bundle into a clone of the upstream
   repo exactly as the PR will.

5. Preview how the listing will read. Paste
   `bundle/manifests/coraza-kubernetes-operator.clusterserviceversion.yaml`
   into <https://operatorhub.io/preview> and check the rendered description,
   icon, categories, maintainer links and CRD examples. This is the same
   metadata reviewers see.

   If something needs fixing, **do not edit the staged bundle**. The fix
   belongs in `bundle/base/csv-template.yaml` on a branch, reviewed and merged
   like any other change, because the staged bundle is regenerated from the
   template on every release and an uncommitted edit is silently reverted on
   the next one. After the template change is merged, return to step 1 and
   redo the generate, validate and preview steps against it.

6. Set credentials for commits and GitHub API access. The publish script
   **requires** `GIT_USER`, `GIT_EMAIL`, and `GITHUB_TOKEN` (a PAT with `repo`
   on the [`networking-incubator`] fork of community-operators). For HTTPS push,
   run `gh auth setup-git` with that PAT (or use credentials your environment
   already provides). `GIT_USER` must be a real name, not a pseudonym: the DCO
   check rejects anonymous contributions.

7. Open the PR to community-operators:

   ```console
   ./hack/publish_operatorhub.sh --version X.Y.Z --fork networking-incubator
   ```

   Use `./hack/publish_operatorhub.sh --help` for options (`--dry-run`, `--fork`,
   etc.). The script copies `bundle/ci.yaml` (from `bundle/base/ci.yaml` via
   `make bundle`) into `operators/<name>/ci.yaml` on the PR branch so this repo
   stays the source of truth for reviewers and `updateGraph`. Reviewers listed
   there may self-merge once CI is green; everyone else waits for a maintainer.

8. Watch the PR on [`k8s-operatorhub/community-operators`] for OperatorHub CI
   feedback until it merges. After the merge the bundle is shipped in
   `quay.io/operatorhubio/catalog` and the listing appears on
   [OperatorHub.io].

[OperatorHub.io]:https://operatorhub.io/

[`operatorhub.yml`]:https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/.github/workflows/operatorhub.yml
[`k8s-operatorhub/community-operators`]:https://github.com/k8s-operatorhub/community-operators
[`networking-incubator`]:https://github.com/networking-incubator/community-operators
