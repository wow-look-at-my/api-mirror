# Drives a real api-mirror's operator surface: the token gates every admin
# path, the gzip wrapper answers a browser that asks, and a caller with no
# credential is refused before any upstream call.
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
	- desc: the dashboard is gated by its token and gzips what a browser asks for
	  timeout: 30s
	  cmd: |
		set -e
		{shared.fakegithub} -listen 127.0.0.1:19941 &
		FAKE_PID=$!
		MIRROR_ADMIN_TOKEN=dats-token GITHUB_API_URL=http://127.0.0.1:19941 {shared.api-mirror} -spec {shared.mirror-spec.xml} -db {outputs.mirror.db} -listen 127.0.0.1:19940 &
		MIRROR_PID=$!
		trap "kill $FAKE_PID $MIRROR_PID 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19941/_requests && curl -s -o /dev/null http://127.0.0.1:19940/nonexistent && break; sleep 0.1; done
		echo "no-token=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19940/_mirror/api/overview)"
		echo "wrong-token=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Mirror-Token: nope' http://127.0.0.1:19940/_mirror/api/overview)"
		echo "token=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Mirror-Token: dats-token' http://127.0.0.1:19940/_mirror/api/overview)"
		echo "encoding=$(curl -s -o /dev/null -D - -H 'Accept-Encoding: gzip' -H 'X-Mirror-Token: dats-token' http://127.0.0.1:19940/_mirror/api/jobs | tr -d '\r' | grep -i '^content-encoding:' | cut -d' ' -f2)"
		echo "whoami=$(curl -s -H 'X-Mirror-Token: dats-token' http://127.0.0.1:19940/_mirror/api/whoami)"
		echo "anonymous-data=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19940/repos/octo/demo)"
		curl -s http://127.0.0.1:19941/_requests > {outputs.upstream-calls.txt}
	  outputs:
		stdout:
			- "no-token=401"
			- "wrong-token=401"
			- "token=200"
			- "encoding=gzip"
			- '"admin":true'
			- "anonymous-data=401"
		files:
			upstream-calls.txt:
				match:
					- "^0$"
