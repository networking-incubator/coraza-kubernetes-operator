# Releases

This documentation will help you build and publish a new release of the Coraza
Kubernetes Operator (CKO).

> **Note**: All releases target tags, and our tags follow [semver].

> **Note**: Most of the release process is automated via [GitHub Workflow]. See
> the [release.yml] workflow for details.

[semver]:https://github.com/semver/semver
[GitHub Workflow]:https://docs.github.com/en/actions/concepts/workflows-and-actions/workflows
[release.yml]:https://github.com/networking-incubator/coraza-kubernetes-operator/blob/main/.github/workflows/release.yml

## Pre-requisites

### Coraza-rs release

Verify whether [coraza-rs](https://github.com/networking-incubator/coraza-rs)
needs a matching release. The CKO release workflow builds the bundled WASM from
the commit pinned by `CORAZA_RS_VERSION` with `WASM_CRS=0`; it does not consume
coraza-rs's prebuilt WASM assets.

If a new coraza-rs release is needed, cut a tag in that repository. Its release
workflow creates:

* `coraza-rs.wasm` — the WASM plugin
* `coraza-rs-crs.wasm` — the plugin with embedded CRS rules

Before tagging CKO, synchronize both sets of Makefile pins:

* `CORAZA_RS_VERSION` is the source commit used by the CKO release workflow. It
  is a Git commit SHA, not an artifact checksum.
* `WASM_RELEASE_VERSION`, `WASM_RELEASE_GIT_SHA`, `WASM_RELEASE_SHA256`, and
  `WASM_RELEASE_CRS_VERSION` pin the published assets used by `make wasm.release`
  and the default `make demo`. These do not affect the CKO release workflow, but
  keep local deployment testing on the same coraza-rs release. The SHA variable
  selects the CRS-enabled asset when `WASM_CRS=1`, otherwise the plain asset.
  `WASM_RELEASE_GIT_SHA` defaults to `CORAZA_RS_VERSION`; override it only when
  testing an artifact built from a different commit.

For coraza-rs `v0.0.3-rc.2`, the tag points to:

```make
CORAZA_RS_VERSION ?= 1caf0a5ae9f7ee0cf83899bb58daa6a33a98bff1
WASM_RELEASE_VERSION ?= v0.0.3-rc.2
WASM_RELEASE_GIT_SHA ?= $(CORAZA_RS_VERSION)
WASM_RELEASE_SHA256 ?= $(if $(filter 1,$(WASM_CRS)),2a4eb39b390439e4ba68c6c57788c9b12113f2a4ba45aa6ae8148e50d93c6dab,40968f7d237065192819b7984cbf0790374e87321caca448154b124f6b3f97d2)
WASM_RELEASE_CRS_VERSION ?= v4.30.0
```

When `CORAZA_RS_VERSION` moves, re-check the gateway capability lists in
`internal/rulesets/validation` - supported operators, directive aliases, and
`ctl` actions - against the new coraza-rs source. Only then update
`gatewayModuleCheckedAt` in `internal/rulesets/validation/operators.go`. The
guard test intentionally fails until that commit is updated:

```console
go test ./internal/rulesets/validation -run TestGatewayListsMatchGatewayModulePin
```

To obtain the published artifact digests when updating the pins, query the
coraza-rs release API. Keep the resulting hashes pinned in the Makefile; do not
fetch them dynamically during a build:

```sh
version=v0.0.3-rc.2
curl -fsSL "https://api.github.com/repos/networking-incubator/coraza-rs/releases/tags/$version" |
  jq -r '.assets[] | select(.name == "coraza-rs.wasm" or .name == "coraza-rs-crs.wasm") | "\(.name) \(.digest | sub("^sha256:"; ""))"'
```

Validate the published artifact with `make demo` before tagging CKO. It deploys
the CRS-enabled artifact on KIND and runs the demo traffic checks. To build from
the pinned coraza-rs source instead, use `make demo WASM_SOURCE=build`; this
requires the Rust toolchain. Both paths overwrite the generated files under
`internal/assets/`, which should not be committed.

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
- The **crds.yaml**, **operator.yaml**, **samples.yaml**, Helm chart **.tgz**, and WASM
  assets (**plugin.wasm**, **plugin.wasm.sha256**, **plugin.manifest.yaml**) are attached
  - Check the rendered image tag and chart metadata (`version` has no `v` prefix;
    `appVersion` matches the image tag, including `v`)
  - Verify the WASM manifest's `git_sha`, `crs_version`, and SHA-256 against the release
- Make sure the **previous release** is set correctly
  - e.g. for a `v1.0.0` release, _don't_ target `rc`, patch or other pre-releases. Target the last major/minor.

Once you've verified the release, we need to tag the container image appropriately _before_ we publish.

[releases page]:https://github.com/networking-incubator/coraza-kubernetes-operator/releases

### Step 4 - Container Image Tagging

The release workflow pushes the operator image tagged as the git tag
(e.g. `v0.1.1`) to GHCR and Quay. When OLM bundle publishing is enabled, the
bundle references the operator image by its immutable digest.

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

**Current status:** the OLM bundle and catalog jobs in `release.yml` are disabled
with `if: false`; the `operatorhub.yml` job is disabled as well. OLM publishing
is not part of the normal image or Helm chart release. The flow below describes
the intended bundle/catalog pipeline if it is re-enabled.

When enabled, the release workflow builds and pushes OLM bundle and catalog
images alongside the operator image:

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

**Intended automated flow (currently disabled):**

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

- If preparing an OLM catalog release, **`catalog.yaml` must be committed before
  tagging**. The catalog job updates it in its working tree but does not commit
  the result back. This is not required for the current Helm/image-only release
  while the OLM jobs remain disabled.

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

**Publish the Helm chart index** — the release workflow triggers Pages while the
release is still a draft, and `pages.yml` excludes drafts. After publishing the
release, trigger Pages again so the chart package is indexed:

```console
gh workflow run pages.yml
```

Published prereleases are included in the index. Verify the chart appears (use
`--devel` to include prereleases in Helm search):

```console
# Run once if the chart repository is not already configured.
helm repo add coraza-kubernetes-operator https://networking-incubator.github.io/coraza-kubernetes-operator/
helm repo update coraza-kubernetes-operator
helm search repo coraza-kubernetes-operator/coraza-kubernetes-operator --versions --devel
```

Merging the post-release PR to `main` also triggers Pages; no extra dispatch is
needed if that run has already published the updated index.

Make sure the latest release announcement is pinned, and older release
announcements get unpinned.

[immutable releases]:https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases

### Step 6 - Post-Release Updates & Github Pages

After the release is published and the container image is available, open a
follow-up PR to `main` with the following changes:

**Bump Chart version** — update `charts/coraza-kubernetes-operator/Chart.yaml`:

```yaml
version: 0.6.0-rc.1        # chart version (without 'v' prefix)
appVersion: "v0.6.0-rc.1"  # operator image tag (matches the release tag)
```

No manual chart bump is needed before tagging: `make release.manifests` renders
the operator manifest with `image.tag` set to the release tag, and packages the
chart with `--version` set to the tag without `v` and `--app-version` set to the
tag with `v`. After the image is published, update the values in the repository
for users installing from a local checkout (e.g. `helm install ./charts/...`).
The chart's `appVersion` is also the default `image.tag`, so it must match the
published image tag, including the `v` prefix.

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

Merging the PR to `main` triggers the [GitHub Pages](https://github.com/networking-incubator/coraza-kubernetes-operator/actions/workflows/pages.yml)
workflow, which rebuilds the docs and Helm chart index. If that run does not
occur, trigger it manually with `gh workflow run pages.yml`.

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

This is separate from the Helm chart release. For a prerelease, skip OperatorHub
submission unless maintainers explicitly request it; the disabled workflow's
planned behavior is to skip prereleases.

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
