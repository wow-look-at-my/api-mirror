# Drives a real api-mirror server with api-cli's own (upstream, not vendored)
# GitHub sample config, against a deterministic fake upstream. Proves the
# whole stack end to end: a genuine independent HTTP client gets correctly
# shaped data on a miss, and a second call is byte-identical with zero new
# upstream requests.
#
# The docker sandbox falls back for us (no bwrap on the CI runner) and runs
# as the host's own uid, so the setup step below can't apt-get its own curl.
# Pin an image that already ships curl + bash instead of a bare debian slim.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		api-mirror: ../build/api-mirror
		fakegithub: ../build/fakegithub
		mirror-spec.xml: ../samples/github/github.xml

setup:
	- cmd: curl -fL --compressed "https://dl.pazer.build/api-cli?os=linux&arch=amd64" -o {shared.api-cli}
	  timeout: 120s
	- cmd: curl -fsSL "https://raw.githubusercontent.com/wow-look-at-my/api-cli/master/samples/github/github.xml" -o {shared.github-cli.xml}
	  timeout: 60s
	- chmod +x {shared.api-cli} {shared.api-mirror} {shared.fakegithub}

tests:
	- desc: api-cli drives api-mirror end to end against a fake GitHub upstream
	  timeout: 30s
	  cmd: |
		set -e
		{shared.fakegithub} -listen 127.0.0.1:19931 &
		FAKE_PID=$!
		GITHUB_API_URL=http://127.0.0.1:19931 {shared.api-mirror} -spec {shared.mirror-spec.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19930 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19931/_requests && curl -s -o /dev/null http://127.0.0.1:19930/nonexistent && break; sleep 0.1; done
		echo "=== miss ==="
		GITHUB_API_URL=http://127.0.0.1:19930 GITHUB_RAW=1 {shared.api-cli} --config {shared.github-cli.xml} repo get octo/demo --as=json
		curl -s http://127.0.0.1:19931/_requests > {outputs.after-miss.txt}
		echo "=== hit ==="
		GITHUB_API_URL=http://127.0.0.1:19930 GITHUB_RAW=1 {shared.api-cli} --config {shared.github-cli.xml} repo get octo/demo --as=json
		curl -s http://127.0.0.1:19931/_requests > {outputs.after-hit.txt}
	  outputs:
		stdout:
			- "=== miss ==="
			- '"default": "main"'
			- '"archived": false'
			- '"private": false'
		!stdout:
			- "error:"
		files:
			after-miss.txt:
				match:
					- "^3$"
			after-hit.txt:
				match:
					- "^3$"
