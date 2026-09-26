#!/usr/bin/env bash
set -euo pipefail

upstream=xiao-qiu-qiu/ClinePassBridge
repo="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
tag="$(gh api "repos/$upstream/releases/latest" --jq .tag_name)"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "::error::Unsupported upstream release tag: $tag"; exit 1;
}
git remote add upstream "https://github.com/$upstream.git"
git fetch origin main custom/main
git fetch upstream 'refs/heads/main:refs/remotes/upstream/main' \
  "refs/tags/$tag:refs/tags/$tag"
git merge-base --is-ancestor origin/main upstream/main || {
  echo '::error::main diverged from upstream. Refusing to force-push the mirror.'
  exit 1
}
git push origin 'refs/remotes/upstream/main:refs/heads/main' "refs/tags/$tag"
git fetch origin main custom/main
mirror_sha="$(git rev-parse origin/main)"
body="${RUNNER_TEMP:?RUNNER_TEMP is required}/upstream-sync.md"
cat > "$body" <<EOF
Synchronize the upstream main branch into custom/main.

- Upstream release marker: $tag
- Upstream main commit: $mirror_sha
- Upstream repository: https://github.com/$upstream

The release marker may precede the main branch head. Merging this PR rebuilds
macOS amd64 and arm64 artifacts and publishes a separate vX.Y.Z-fork.N release.
EOF

if git merge-base --is-ancestor origin/main origin/custom/main; then
  echo 'The upstream mirror is already included in custom/main.'
  # Retry dispatch if a previous sync stopped after merging but before dispatch.
  number="$(gh pr list --repo "$repo" --state merged --base custom/main \
    --head main --limit 1 --json number --jq '.[0].number // empty')"
else
  number="$(gh pr list --repo "$repo" --state open --base custom/main \
    --head main --json number --jq '.[0].number // empty')"
  if [[ -z "$number" ]]; then
    gh pr create --repo "$repo" --base custom/main --head main \
      --title "chore: sync upstream $tag" --body-file "$body"
    number="$(gh pr list --repo "$repo" --state open --base custom/main \
      --head main --json number --jq '.[0].number')"
  else
    gh pr edit "$number" --repo "$repo" \
      --title "chore: sync upstream $tag" --body-file "$body"
  fi
  if [[ "$(gh api "repos/$repo" --jq .allow_auto_merge)" == true ]]; then
    if ! gh pr merge "$number" --repo "$repo" --auto --merge \
      --match-head-commit "$mirror_sha"; then
      echo '::warning::Auto-merge was denied or the PR conflicts. The sync PR remains open.'
    fi
  else
    echo '::warning::Repository auto-merge is disabled. Merge the sync PR manually.'
  fi
fi

if [[ -n "${number:-}" ]]; then
  gh pr view "$number" --repo "$repo" --json url,state --jq '(.url + " (" + .state + ")")' \
    >> "$GITHUB_STEP_SUMMARY"
  if [[ "$(gh pr view "$number" --repo "$repo" --json state --jq .state)" == MERGED ]]; then
    # Events produced by GITHUB_TOKEN do not start ordinary downstream workflows.
    # workflow_dispatch is explicitly supported; release preparation deduplicates by SHA.
    gh workflow run release-on-custom-merge.yml --repo "$repo" --ref custom/main \
      -f "pr_number=$number"
  fi
fi
printf 'Upstream release: `%s`\nMirror commit: `%s`\n' "$tag" "$mirror_sha" \
  >> "$GITHUB_STEP_SUMMARY"
