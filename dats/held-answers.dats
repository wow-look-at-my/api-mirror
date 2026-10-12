# Each is asked twice, and the second must be a hit that reaches no
# upstream.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		api-mirror: ../build/api-mirror
		fakegithub: ../build/fakegithub
		github.xml: ../samples/github/github.xml

setup:
	- chmod +x {shared.api-mirror} {shared.fakegithub}

tests:
	- desc: redirects, text, refusals and flags are held, and a second read reaches no upstream
	  timeout: 60s
	  cmd: |
		{shared.fakegithub} -listen 127.0.0.1:19981 &
		FAKE_PID=$!
		GITHUB_API_URL=http://127.0.0.1:19981 {shared.api-mirror} -spec {shared.github.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19980 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19981/_requests && curl -s -o /dev/null http://127.0.0.1:19980/.well-known/docker-updater/health && break; sleep 0.1; done
		M=http://127.0.0.1:19980
		ask() { curl -s -o /dev/null -D - -H 'Authorization: token fake' "$@" | tr -d '\r' | tr '[:upper:]' '[:lower:]' | grep -E '^(http|location|x-gsm-cache|content-type)' | sed 's/^/  /'; }
		for path in /repos/Octo/Demo/actions/runs/5/logs /repos/Octo/Demo/actions/jobs/6/logs /repos/sqlc-dev/sqlc/tarball/cmd/v1.28.0 /repos/sqlc-dev/sqlc/zipball/cmd/sqlc/v1.28.0; do
			echo "== $path"; ask "$M$path"; ask "$M$path"
		done
		echo "== diff"
		ask -H 'Accept: application/vnd.github.diff' $M/repos/Octo/Demo/pulls/7
		curl -s -H 'Authorization: token fake' -H 'Accept: application/vnd.github.v3.diff' $M/repos/Octo/Demo/pulls/7 | head -1
		echo "== blocked"
		ask $M/repos/Octo/Blocked/readme
		curl -s -H 'Authorization: token fake' $M/repos/Octo/Blocked/readme; echo
		echo "== gh accept"
		ask -H 'Accept: application/vnd.github.merge-info-preview+json, application/vnd.github.nebula-preview' $M/repos/Octo/Demo/actions/workflows/3
		echo "== graphql"
		for i in 1 2; do
			echo "graphql $i: $(ask -X POST -H 'Content-Type: application/json' -H 'Accept: application/vnd.github.merge-info-preview+json, application/vnd.github.nebula-preview' \
				--data '{"query":"query($o:String!,$n:String!){repository(owner:$o,name:$n){isFork parent{nameWithOwner}}}","variables":{"o":"Octo","n":"Demo"}}' $M/graphql | grep x-gsm-cache)"
		done
		echo "== exclude"
		curl -s -H 'Authorization: token fake' "$M/repos/Octo/Demo/actions/runs/5?exclude_pull_requests=true" | grep -o '"pull_requests":\[[^]]*\]'
		curl -s -H 'Authorization: token fake' "$M/repos/Octo/Demo/actions/runs/5" | grep -o '"pull_requests":\[{' | head -1
		before=$(curl -s http://127.0.0.1:19981/_requests)
		for path in /repos/Octo/Demo/actions/runs/5/logs /repos/sqlc-dev/sqlc/tarball/cmd/v1.28.0 /repos/Octo/Blocked/readme /repos/Octo/Demo/actions/runs/5; do
			curl -s -o /dev/null -H 'Authorization: token fake' "$M$path"
		done
		curl -s -o /dev/null -H 'Authorization: token fake' -H 'Accept: application/vnd.github.diff' $M/repos/Octo/Demo/pulls/7
		echo "upstream calls on warm reads: $(( $(curl -s http://127.0.0.1:19981/_requests) - before ))"
	  outputs:
		stdout:
			- "location: https://results.example/logs/run/5?se="
			- "location: https://results.example/logs/job/6?se="
			- "location: https://codeload.example/sqlc-dev/sqlc/legacy.tar.gz/refs/tags/cmd/v1.28.0"
			- "location: https://codeload.example/sqlc-dev/sqlc/legacy.zip/refs/tags/cmd/sqlc/v1.28.0"
			- "x-gsm-cache: hit"
			- "content-type: application/vnd.github.diff; charset=utf-8"
			- "diff --git a/README.md b/README.md"
			- "http/1.1 451"
			- "Repository access blocked"
			- "x-gsm-cache: miss"
			- "\"pull_requests\":[]"
			- "\"pull_requests\":[{"
			- "upstream calls on warm reads: 0"
			- "graphql 2:   x-gsm-cache: hit"
		!stdout:
			- "x-gsm-cache: passthrough"
