# A request with no caller signs in as the GitHub App, the way
# github-state-mirror does. The shipped spec exchanges a JWT it signs for the
# installation token of the row's owner. A caller's own request never carries
# that token. The consistency check is the request with no caller here,
# because it runs on demand rather than on the sweep's timer.
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
	- desc: a request with no caller carries the owner's installation token, and a caller's request does not
	  timeout: 30s
	  cmd: |
		set -e
		{shared.fakegithub} -listen 127.0.0.1:19971 -app-id 4242 -app-key {outputs.app.pem} &
		FAKE_PID=$!
		trap 'kill $FAKE_PID $MIRROR_PID 2>/dev/null' EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19971/_requests && break; sleep 0.1; done
		GITHUB_API_URL=http://127.0.0.1:19971 GITHUB_APP_ID=4242 GITHUB_APP_PRIVATE_KEY_PATH={outputs.app.pem} \
			MIRROR_ADMIN_TOKEN=admin WEBHOOK_REPLAY_INTERVAL=0 \
			{shared.api-mirror} -spec {shared.github.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19970 2> {outputs.mirror.log} &
		MIRROR_PID=$!
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19970/.well-known/docker-updater/health && break; sleep 0.1; done
		curl -s -o /dev/null http://127.0.0.1:19970/repos/octo/demo
		echo "caller=[$(curl -s http://127.0.0.1:19971/_auth)]"
		echo "exchanges-after-caller=$(curl -s http://127.0.0.1:19971/_exchanges)"
		# The spec requires the caller's Authorization on the data plane, so the
		# row the check sweeps comes from a caller who has one. The check itself
		# is the mirror's own call, and that one carries the App's token.
		curl -s -o /dev/null -H 'Authorization: token seed' http://127.0.0.1:19970/repos/octo/demo
		echo "check-1=$(curl -s "http://127.0.0.1:19970/_mirror/api/check?kind=repo&token=admin")"
		echo "check=[$(curl -s http://127.0.0.1:19971/_auth)]"
		echo "check-2=$(curl -s "http://127.0.0.1:19970/_mirror/api/check?kind=repo&token=admin")"
		echo "exchanges-after-checks=$(curl -s http://127.0.0.1:19971/_exchanges)"
		echo "announced=$(grep -c 'credential app: on' {outputs.mirror.log})"
	  outputs:
		stdout:
			- "caller=[]"
			- "exchanges-after-caller=0"
			- '"verdicts":{"agrees":1}'
			- "check=[token ghs_fake_installation_"
			# The token lives an hour, so the second check reuses it.
			- "exchanges-after-checks=1"
			- "announced=1"
		!stdout:
			- "unreachable"
