# The launch contract a container depends on. The image runs the binary with
# a mounted spec. A database on a volume, and a listen address, and
# docker-updater polls both health paths the spec declares. Neither path may
# fall through to the upstream.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		api-mirror: ../build/api-mirror
		github.xml: ../samples/github/github.xml

setup:
	- chmod +x {shared.api-mirror}

tests:
	- desc: api-mirror serves the GitHub spec and answers docker-updater's health paths
	  timeout: 30s
	  cmd: |
		set -e
		GITHUB_API_URL=http://127.0.0.1:1 {shared.api-mirror} --spec {shared.github.xml} --db {outputs.mirror.db} --listen 127.0.0.1:19940 &
		PID=$!
		trap "kill $PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19940/.well-known/docker-updater/health && break; sleep 0.1; done
		echo "health=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19940/.well-known/docker-updater/health)"
		echo "pre-update=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19940/.well-known/docker-updater/pre-update)"
		test -f {outputs.mirror.db} && echo "db=created"
	  outputs:
		stdout:
			- "health=200"
			- "pre-update=200"
			- "db=created"
