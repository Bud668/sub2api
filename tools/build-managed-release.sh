#!/usr/bin/env bash
# Build only. Promotion/deployment requires the manual gates in RELEASE-POLICY.md.
set -Eeuo pipefail
umask 077

project_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[[ $# -ge 2 && $# -le 3 ]] || { echo "用法：$0 <持久化源码git仓库> <不存在的产物目录> [manifest.json]" >&2; exit 1; }
upstream_dir=$(realpath -e "$1")
release_dir=$(realpath -m "$2")
manifest=$(realpath -e "${3:-$project_dir/config/managed-release.json}")
fail() { echo "$*" >&2; exit 1; }

# Parse data, never source/eval a release manifest.
jq -e '
  (.upstream_tag | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) and
  (.upstream_commit | test("^[0-9a-f]{40}$")) and
  (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+(-[A-Za-z0-9.]+)?$")) and
  (.commit | test("^[A-Za-z0-9.-]+$")) and
  (.build_date | test("^[0-9TZ:-]+$")) and
  (if has("source_commit") then
    (.source_commit | test("^[0-9a-f]{40}$")) and
    (.source_repo | test("^https://github.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$"))
  else
    (.patch | test("^patches/[A-Za-z0-9._-]+\\.patch$")) and
    (.patch_sha256 | test("^[0-9a-f]{64}$"))
  end)
' "$manifest" >/dev/null || fail "发布清单无效"
tag=$(jq -r .upstream_tag "$manifest")
base=$(jq -r .upstream_commit "$manifest")
version=$(jq -r .version "$manifest")
commit=$(jq -r .commit "$manifest")
build_date=$(jq -r .build_date "$manifest")
source_commit=$(jq -r '.source_commit // empty' "$manifest")
model_quota_release=$(jq -r '.features.user_model_request_quotas // false' "$manifest")
dynamic_quota_release=$(jq -r '.features.dynamic_subscription_quotas // false' "$manifest")
if [[ "$model_quota_release" == true || "$dynamic_quota_release" == true ]]; then
    [[ -n ${SUB2API_MODEL_QUOTA_TEST_DSN:-} ]] || fail "用户模型配额发布必须提供隔离 PostgreSQL 的 SUB2API_MODEL_QUOTA_TEST_DSN；测试代码拒绝生产库名"
fi
[[ $(git -C "$upstream_dir" rev-parse "refs/tags/$tag^{commit}") == "$base" ]] || fail "官方 tag/commit 不一致"
if [[ -n "$source_commit" ]]; then
    [[ $(git -C "$upstream_dir" cat-file -t "$source_commit") == commit && "$commit" == "$source_commit" ]] || fail "源码提交不存在或构建提交不一致"
    git -C "$upstream_dir" merge-base --is-ancestor "$base" "$source_commit" || fail "源码不包含声明的官方基线"
else
    patch_file="$project_dir/$(jq -r .patch "$manifest")"
    [[ $(sha256sum "$patch_file" | cut -d ' ' -f 1) == "$(jq -r .patch_sha256 "$manifest")" ]] || fail "补丁 SHA-256 不一致"
fi
[[ $(node --version) == "$(jq -r .node_version "$manifest")" ]] || fail "Node 版本不一致"
[[ $(pnpm --version) == "$(jq -r .pnpm_version "$manifest")" ]] || fail "pnpm 版本不一致"

mkdir "$release_dir" # Refuse to reuse or overwrite an existing release.
exec > >(tee "$release_dir/build.log") 2>&1
if [[ -n "$source_commit" ]]; then
    git -C "$upstream_dir" worktree add --detach "$release_dir/source" "$source_commit"
    git -C "$upstream_dir" archive --format=tar.gz --prefix=sub2api/ "$source_commit" > "$release_dir/source.tar.gz"
    git -C "$upstream_dir" diff --binary "$base" "$source_commit" > "$release_dir/source-changes.patch"
    patch_name=source-changes.patch
else
    git -C "$upstream_dir" worktree add --detach "$release_dir/source" "$base"
    git -C "$release_dir/source" apply --check "$patch_file"
    git -C "$release_dir/source" apply "$patch_file"
    cp "$patch_file" "$release_dir/"
    patch_name=$(basename "$patch_file")
fi
(
    cd "$release_dir/source/backend"
    if [[ -f migrations/238_dynamic_subscription_quotas.sql ]]; then
        [[ "$dynamic_quota_release" == true ]] || fail "动态额度源码必须在候选清单声明 dynamic_subscription_quotas 并通过隔离数据库回归"
    elif [[ "$dynamic_quota_release" == true ]]; then
        fail "清单声明动态额度，但固定源码不包含对应迁移"
    fi
    [[ $(go env GOVERSION) == "$(jq -r .go_version "$manifest")" ]] || fail "Go 版本不一致"
    go test ./internal/service ./internal/service/openai_ws_v2 ./internal/handler ./internal/handler/admin \
        ./internal/server/... ./internal/repository ./internal/pkg/apicompat ./internal/pkg/openai \
        ./internal/pkg/requestmodel ./internal/pkg/httputil ./migrations
	go test -tags=unit ./internal/server/middleware \
		-run 'CyberSuspension' -count=1
    go test -race ./internal/service ./internal/service/openai_ws_v2 ./internal/handler ./internal/repository \
        -run 'Cyber|RedactContentModeration|Relay|Passthrough|HTTPBridge|ModelAllowlist|AuthCacheInvalidation' -count=1
	go test -race -tags=unit ./internal/server/middleware \
		-run 'CyberSuspension' -count=100
    if [[ "$model_quota_release" == true ]]; then
        go test -race ./internal/service ./internal/handler ./internal/server/middleware \
            -run 'UserModel' -count=2
    fi
    if [[ "$dynamic_quota_release" == true ]]; then
        go test -race ./internal/service ./internal/handler/... ./internal/server/... ./internal/repository \
            -run 'DynamicQuota' -count=2
    fi
)
(
    cd "$release_dir/source/frontend"
    pnpm install --frozen-lockfile
    pnpm exec vitest run src/views/admin/__tests__/groupModelAllowlist.spec.ts \
        src/views/admin/__tests__/groupModelAllowlistLayout.spec.ts \
        src/views/admin/__tests__/GroupsView.codexManifest.spec.ts \
        src/views/admin/__tests__/groupsReasoningEffort.spec.ts \
		src/views/admin/__tests__/RiskControlView.spec.ts \
		src/components/admin/group/__tests__/ReasoningEffortPolicyFields.spec.ts \
        src/views/user/__tests__/UsageView.spec.ts
    if [[ "$model_quota_release" == true ]]; then
        pnpm exec vitest run src/api/__tests__/modelPolicy.spec.ts \
            src/components/account/__tests__/ModelWhitelistSelector.spec.ts \
            src/components/account/__tests__/CreateAccountModal.spec.ts \
            src/components/account/__tests__/EditAccountModal.spec.ts \
            src/components/account/__tests__/BulkEditAccountModal.spec.ts \
            src/components/admin/user/__tests__/UserModelPolicyModal.spec.ts \
            src/components/user/__tests__/UserModelQuotaStatus.spec.ts \
            src/views/admin/__tests__/UsersView.spec.ts \
            src/i18n/__tests__/localeKeyCompleteness.spec.ts \
            src/i18n/__tests__/localesMessageCompile.spec.ts \
            src/i18n/__tests__/localesNoKeyCollision.spec.ts
    fi
    pnpm run build
    if [[ "$dynamic_quota_release" == true ]]; then
        pnpm exec vitest run src/components/admin/__tests__/DynamicQuotaDialog.spec.ts \
            src/components/common/__tests__/DynamicQuotaCard.spec.ts \
            src/views/admin/__tests__/SubscriptionsView.userUsageLink.spec.ts
    fi
)
(
    cd "$release_dir/source/backend"
    CGO_ENABLED=0 go build -tags=embed -trimpath \
        -ldflags="-s -w -X main.Version=$version -X main.Commit=$commit -X main.Date=$build_date -X main.BuildType=release" \
        -o "$release_dir/sub2api" ./cmd/server
)
"$release_dir/sub2api" --version
binary_hash=$(sha256sum "$release_dir/sub2api" | cut -d ' ' -f 1)
jq --arg hash "$binary_hash" '.status="candidate" | .binary_sha256=$hash |
  .verification={go_tests:"passed",race_tests:"passed",frontend_build:"passed"}' \
    "$manifest" > "$release_dir/managed-release.json"
(
    cd "$release_dir"
    sha256sum sub2api managed-release.json "$patch_name" > SHA256SUMS
    if [[ -n "$source_commit" ]]; then sha256sum source.tar.gz >> SHA256SUMS; fi
)
echo "构建和回归通过：$release_dir；尚未部署，仍需迁移审查、加密备份与主备验收。"
