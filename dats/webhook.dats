# A signed push delivery states a branch tip, and the next read of that branch
# answers it from the delivery: the only upstream call is the reveal probe.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		api-mirror: ../build/api-mirror
		fakegithub: ../build/fakegithub
		mirror-spec.xml: ../samples/github/github.xml

setup:
	- chmod +x {shared.api-mirror} {shared.fakegithub}

tests:
	- desc: a signed push is applied and served without a fetch; an unsigned one is refused
	  timeout: 30s
	  cmd: |
		set -e
		{shared.fakegithub} -listen 127.0.0.1:19951 &
		FAKE_PID=$!
		WEBHOOK_SECRET=dats-secret GITHUB_API_URL=http://127.0.0.1:19951 {shared.api-mirror} -spec {shared.mirror-spec.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19950 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19951/_requests && curl -s -o /dev/null http://127.0.0.1:19950/nonexistent && break; sleep 0.1; done
		BODY='{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111","after":"2222222222222222222222222222222222222222","deleted":false,"repository":{"full_name":"octo/demo","name":"demo","owner":{"login":"octo"},"pushed_at":1787000000}}'
		SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac dats-secret | sed 's/^.*= //')"
		echo "unsigned=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-GitHub-Event: push' -H 'Content-Type: application/json' --data "$BODY" http://127.0.0.1:19950/webhook)"
		echo "signed=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'X-GitHub-Event: push' -H "X-Hub-Signature-256: $SIG" -H 'Content-Type: application/json' --data "$BODY" http://127.0.0.1:19950/webhook)"
		sleep 3
		echo "branch=$(curl -s -H 'Authorization: Bearer fake-token' -H 'Accept: application/vnd.github+json' http://127.0.0.1:19950/repos/octo/demo/git/ref/heads/main)"
		curl -s http://127.0.0.1:19951/_requests > {outputs.upstream-calls.txt}
	  outputs:
		stdout:
			- "unsigned=403"
			- "signed=202"
			- "2222222222222222222222222222222222222222"
			- "refs/heads/main"
		files:
			upstream-calls.txt:
				match:
					- "^1$"
