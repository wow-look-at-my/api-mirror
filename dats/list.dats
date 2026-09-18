# A list route answers the upstream's bare array, keys each distinct page
# apart, serves a repeat from storage, and refetches after a push moves it.
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
	- desc: a branch list is cached per page and dropped by a push
	  timeout: 30s
	  cmd: |
		set -e
		{shared.fakegithub} -listen 127.0.0.1:19961 &
		FAKE_PID=$!
		WEBHOOK_SECRET=dats-secret GITHUB_API_URL=http://127.0.0.1:19961 {shared.api-mirror} -spec {shared.mirror-spec.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19960 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19961/_requests && curl -s -o /dev/null http://127.0.0.1:19960/nonexistent && break; sleep 0.1; done
		get() { curl -s -H 'Authorization: Bearer fake-token' -H 'Accept: application/vnd.github+json' "http://127.0.0.1:19960/repos/octo/demo/branches$1"; }
		calls() { echo "$(curl -s http://127.0.0.1:19961/_requests) $(curl -s http://127.0.0.1:19961/_log)"; }
		echo "miss=$(get '')"
		echo "calls-after-miss=$(calls)"
		echo "hit=$(get '')"
		echo "calls-after-hit=$(calls)"
		get '?per_page=1' > /dev/null
		echo "calls-after-other-page=$(calls)"
		BODY='{"ref":"refs/heads/main","before":"1111111111111111111111111111111111111111","after":"2222222222222222222222222222222222222222","deleted":false,"repository":{"full_name":"octo/demo","name":"demo","owner":{"login":"octo"},"pushed_at":1787000000}}'
		SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac dats-secret | sed 's/^.*= //')"
		curl -s -o /dev/null -X POST -H 'X-GitHub-Event: push' -H "X-Hub-Signature-256: $SIG" -H 'Content-Type: application/json' --data "$BODY" http://127.0.0.1:19960/webhook
		sleep 3
		get '' > /dev/null
		echo "calls-after-push=$(calls)"
	  outputs:
		stdout:
			0: '^miss=\[\{.*3{40}'
			1: "^calls-after-miss=2 "
			2: '^hit=\[\{.*3{40}'
			3: "^calls-after-hit=2 "
			4: "^calls-after-other-page=3 "
			5: "^calls-after-push=4 "
