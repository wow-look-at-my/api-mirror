# Holds every cached column route of the GitHub sample against the upstream's
# own answer to the same request, cold and then warm. A served answer must be a
# subset of the upstream's: same paths, same values, same types. The unit tests
# check the spec against itself; only this suite checks it against the upstream.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		api-mirror: ../build/api-mirror
		fakegithub: ../build/fakegithub
		github.xml: ../samples/github/github.xml
		upstream-shape.yaml: upstream-shape.yaml

# The same CA-bundle workaround as api-cli-smoke.dats: the sandbox mounts only
# the system trust store.
setup:
	- cmd: env -u CURL_CA_BUNDLE -u SSL_CERT_FILE -u REQUESTS_CA_BUNDLE curl -fL --compressed "https://dl.pazer.build/api-mirror-delta?os=linux&arch=amd64" -o {shared.api-mirror-delta}
	  timeout: 120s
	- chmod +x {shared.api-mirror-delta} {shared.api-mirror} {shared.fakegithub}

tests:
	- desc: every cached column route answers a subset of the upstream, on a miss and on a hit
	  timeout: 60s
	  cmd: |
		{shared.fakegithub} -listen 127.0.0.1:19961 &
		FAKE_PID=$!
		GITHUB_API_URL=http://127.0.0.1:19961 {shared.api-mirror} -spec {shared.github.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19960 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19961/_requests && curl -s -o /dev/null http://127.0.0.1:19960/.well-known/docker-updater/health && break; sleep 0.1; done
		for round in miss hit; do
			echo "=== $round ==="
			{shared.api-mirror-delta} compare -c {shared.upstream-shape.yaml} \
				--truth http://127.0.0.1:19961 --mirror api-mirror=http://127.0.0.1:19960 \
				-H 'Authorization: token fake' \
				/repos/Octo/Demo \
				/repos/Octo/Demo/pulls \
				/repos/Octo/Demo/pulls/7 \
				/repos/Octo/Demo/git/ref/heads/main \
				/repos/Octo/Demo/commits/abc123/statuses \
				/repos/Octo/Demo/statuses/abc123 \
				/repos/Octo/Demo/git/commits/abc123
			echo "exit=$?"
		done
	  outputs:
		stdout:
			- "=== miss ==="
			- "=== hit ==="
			- "exchanges 7  matched 7  differing 0  errors 0"
		!stdout:
			- "exit=1"
			- "exit=2"
			- "differences:"
