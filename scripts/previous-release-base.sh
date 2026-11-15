# shellcheck shell=bash
# shellcheck disable=SC2034 # base_commit, base_desc and base_rule are read by the sourcing script.
# Chooses the previous release that scripts/test-previous-release-schema.sh
# checks the target against. Sourced, never run. select_previous_release sets
# base_commit, base_desc and base_rule; each rule it passes over is reported
# on stderr, so the job log names the rule that decided and why.
#
# The rules, the first that applies wins:
#   1. an explicit base argument (a commit or tag); it skips every other rule
#   2. the deploy image: the revision annotation on the registry tag
#      ${HK_FOLLOW_IMAGE}:${HK_FOLLOW_TAG} (ghcr.io/mgh3326/handoffkeep:deploy).
#      hk-follow moves that tag on every production deploy, so it names what
#      production runs (deploy/hk-follow/README.md, step 6). It counts when it
#      names a commit in this checkout that is strictly before the target.
#   3. the `deploy` git tag, under the same condition. Push deploy no longer
#      moves it, so it can be older than what production runs.
#   4. the newest v* tag reachable from the target, excluding the target's own
#      tag. v0.0.0 is auto-tag's placeholder, never a release.
#
# A registry read that fails is not an error: it passes over rule 2 and the
# job goes on with rule 3 or 4. The registry is read anonymously (the package
# is public, hk-follow I6), so no credential is needed.

# deploy_image_ref: the registry tag rule 2 reads, for logs and base_desc.
deploy_image_ref() {
	printf '%s:%s' "${HK_FOLLOW_IMAGE:-ghcr.io/mgh3326/handoffkeep}" "${HK_FOLLOW_TAG:-deploy}"
}

# registry_revision: print the revision annotation on the deploy image tag.
# rc 0 with the sha on stdout; 3 when the tag does not exist; 4 when the tag
# carries no valid revision; 1 for any other failure. The read is hk-follow's
# (registry_manifest and resolve): an anonymous pull token, then the manifest,
# whose index carries org.opencontainers.image.revision.
registry_revision() {
	local image=${HK_FOLLOW_IMAGE:-ghcr.io/mgh3326/handoffkeep}
	local tag=${HK_FOLLOW_TAG:-deploy}
	local host=${image%%/*} repo=${image#*/}
	local token response code body rev
	token=$(curl -fsS --max-time 20 "https://${host}/token?scope=repository:${repo}:pull" |
		python3 -c 'import json, sys; print(json.load(sys.stdin).get("token", ""))' 2>/dev/null) || return 1
	[[ -n "$token" ]] || return 1
	response=$(curl -sS --max-time 20 -w '\n%{http_code}' \
		-H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"https://${host}/v2/${repo}/manifests/${tag}") || return 1
	code=${response##*$'\n'}
	body=${response%$'\n'*}
	case $code in
	200) ;;
	404) return 3 ;;
	*) return 1 ;;
	esac
	rev=$(printf '%s' "$body" | python3 -c 'import json, sys; print((json.load(sys.stdin).get("annotations") or {}).get("org.opencontainers.image.revision", ""))' 2>/dev/null) || return 4
	[[ "$rev" =~ ^[0-9a-f]{40}$ ]] || return 4
	printf '%s\n' "$rev"
}

# select_previous_release <target-commit> [base-input]
select_previous_release() {
	local target_commit=$1 base_input=${2:-}
	local rc=0 rev reason deploy_commit tags tag
	base_commit=''
	base_desc=''
	base_rule=''

	if [[ -n "$base_input" ]]; then
		git cat-file -e "$base_input^{commit}" 2>/dev/null || {
			printf "schema-compat: base '%s' is not a commit in this checkout\n" "$base_input" >&2
			return 1
		}
		base_commit=$(git rev-parse --verify "$base_input^{commit}")
		if [[ "$base_commit" == "$target_commit" ]]; then
			printf "schema-compat: base '%s' is the target; there is no previous release to check\n" "$base_input" >&2
			return 1
		fi
		base_desc=$base_input
		base_rule='explicit argument'
		return 0
	fi

	# Rule 2: the deploy image revision.
	rev=''
	rc=0
	rev=$(registry_revision) || rc=$?
	if ((rc == 0)); then
		if [[ ! "$rev" =~ ^[0-9a-f]{40}$ ]]; then
			reason="deploy image tag $(deploy_image_ref) carries no valid revision"
		elif [[ "$rev" == "$target_commit" ]]; then
			reason="deploy image $(deploy_image_ref) names the target itself"
		elif ! git cat-file -e "$rev^{commit}" 2>/dev/null; then
			reason="deploy image $(deploy_image_ref) names $rev, which is not in this checkout"
		elif ! git merge-base --is-ancestor "$rev" "$target_commit"; then
			reason="deploy image $(deploy_image_ref) names $rev, which is not an ancestor of the target"
		else
			base_commit=$rev
			base_desc="$(deploy_image_ref) ($rev)"
			base_rule='deploy image tag (its revision is an ancestor of the target)'
		fi
	elif ((rc == 3)); then
		reason="no deploy image tag $(deploy_image_ref)"
	elif ((rc == 4)); then
		reason="deploy image tag $(deploy_image_ref) carries no valid revision"
	else
		reason="deploy image tag $(deploy_image_ref) could not be read from the registry"
	fi
	if [[ -z "$base_commit" && -n "${reason:-}" ]]; then
		printf 'schema-compat: deploy image rule passed over: %s; trying the deploy git tag\n' "$reason" >&2
	fi

	# Rule 3: the deploy git tag, when it names a commit strictly before the target.
	if [[ -z "$base_commit" ]]; then
		if deploy_commit=$(git rev-parse -q --verify 'refs/tags/deploy^{commit}'); then
			if [[ "$deploy_commit" != "$target_commit" ]] && git merge-base --is-ancestor "$deploy_commit" "$target_commit"; then
				base_commit=$deploy_commit
				base_desc=deploy
				base_rule='deploy tag (an ancestor of the target)'
			else
				printf 'schema-compat: deploy git tag passed over: it does not name a commit strictly before the target; trying the newest v* tag\n' >&2
			fi
		else
			printf 'schema-compat: deploy git tag passed over: there is no deploy tag; trying the newest v* tag\n' >&2
		fi
	fi

	# Rule 4: the newest released v* tag reachable from the target.
	if [[ -z "$base_commit" ]]; then
		tags=$(git tag --merged "$target_commit" --sort=-v:refname)
		for tag in $tags; do
			[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
			[[ $tag == v0.0.0 ]] && continue
			[[ "$(git rev-parse --verify "$tag^{commit}")" == "$target_commit" ]] && continue
			base_commit=$(git rev-parse --verify "$tag^{commit}")
			base_desc=$tag
			base_rule='newest v* tag before the target'
			break
		done
		if [[ -z "$base_commit" ]]; then
			printf 'schema-compat: no v* release tag before %s; pass its commit as the argument\n' "$target_commit" >&2
			return 1
		fi
	fi
	return 0
}
