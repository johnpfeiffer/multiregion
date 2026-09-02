#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat >&2 <<'EOF'
Usage: scripts/set-api-token.sh [API_TOKEN]

Updates API_TOKEN on the Lambda functions in Svc-usw2 and Svc-use1 while
preserving their other environment variables. If API_TOKEN is omitted, the
script prompts for it without echoing.

AWS_PROFILE and the normal AWS CLI environment variables are honored.
EOF
}

case $# in
	0)
		if [[ ! -t 0 ]]; then
			usage
			exit 2
		fi
		read -r -s -p "API token: " api_token
		printf '\n'
		;;
	1)
		api_token=$1
		;;
	*)
		usage
		exit 2
		;;
esac

if (( ${#api_token} < 32 )); then
	echo "API token must contain at least 32 characters" >&2
	exit 2
fi
if [[ "$api_token" =~ [[:space:]] ]]; then
	echo "API token must not contain whitespace" >&2
	exit 2
fi

command -v aws >/dev/null || { echo "AWS CLI v2 is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

temp_dir=$(mktemp -d)
chmod 700 "$temp_dir"
trap 'rm -rf -- "$temp_dir"' EXIT

token_file="$temp_dir/token"
printf '%s' "$api_token" >"$token_file"
chmod 600 "$token_file"

update_region() {
	local stack_name=$1
	local region=$2
	local function_name variables_file environment_file

	function_name=$(aws cloudformation describe-stacks \
		--stack-name "$stack_name" \
		--region "$region" \
		--query "Stacks[0].Outputs[?OutputKey=='LambdaName'].OutputValue" \
		--output text)
	if [[ -z "$function_name" || "$function_name" == "None" ]]; then
		echo "$stack_name in $region has no LambdaName output; deploy the current stacks first" >&2
		exit 1
	fi

	variables_file="$temp_dir/variables-$region.json"
	environment_file="$temp_dir/environment-$region.json"
	aws lambda get-function-configuration \
		--function-name "$function_name" \
		--region "$region" \
		--query 'Environment.Variables' \
		--output json >"$variables_file"

	jq -n \
		--slurpfile variables "$variables_file" \
		--rawfile token "$token_file" \
		'{Variables: (($variables[0] // {}) + {API_TOKEN: $token})}' \
		>"$environment_file"

	aws lambda update-function-configuration \
		--function-name "$function_name" \
		--region "$region" \
		--environment "file://$environment_file" \
		--query FunctionName \
		--output text >/dev/null

	aws lambda wait function-updated-v2 \
		--function-name "$function_name" \
		--region "$region"

	echo "Updated API_TOKEN for $function_name in $region"
}

update_region Svc-usw2 us-west-2
update_region Svc-use1 us-east-1
unset api_token
