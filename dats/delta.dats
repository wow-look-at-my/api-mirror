# Drives the built binary against fixture servers that disagree in known
# ways. The suite covers the one-shot compare, its exit codes, and the proxy.
sandbox:
	image: buildpack-deps:bookworm-curl

shared:
	copy:
		delta: ../build/api-mirror-delta
		fixtureserver: ../build/fixtureserver
		truth-demo.json: fixtures/truth-demo.json
		mirror-demo.json: fixtures/mirror-demo.json
		truth-same.json: fixtures/truth-same.json
		mirror-same.json: fixtures/mirror-same.json
		ignore.yaml: fixtures/ignore.yaml

setup:
	- chmod +x {shared.delta} {shared.fixtureserver}

tests:
	- desc: compare reports every difference and exits 1
	  timeout: 30s
	  cmd: |
		{shared.fixtureserver} -listen 127.0.0.1:19951 -route /repos/o/demo={shared.truth-demo.json} -route /same={shared.truth-same.json} &
		T=$!
		{shared.fixtureserver} -listen 127.0.0.1:19952 -route /repos/o/demo={shared.mirror-demo.json} -route /same={shared.mirror-same.json} &
		M=$!
		trap "kill $T $M 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19951/same && curl -s -o /dev/null http://127.0.0.1:19952/same && break; sleep 0.1; done
		{shared.delta} compare --truth http://127.0.0.1:19951 --mirror local=http://127.0.0.1:19952 /repos/o/demo /same
		echo "exit=$?"
	  outputs:
		stdout:
			- "GET /repos/o/demo [local] truth=200 mirror=200: 3 differences"
			- "missing  $.html_url"
			- "missing  $.owner.avatar_url"
			- "changed  $.stargazers_count"
			- "GET /same [local] truth=200 mirror=200: match"
			- "exchanges 2  matched 1  differing 1"
			- "exit=1"

	- desc: ignore rules hide what they name, and compare exits 0
	  timeout: 30s
	  cmd: |
		{shared.fixtureserver} -listen 127.0.0.1:19953 -route /repos/o/demo={shared.truth-demo.json} &
		T=$!
		{shared.fixtureserver} -listen 127.0.0.1:19954 -route /repos/o/demo={shared.mirror-demo.json} &
		M=$!
		trap "kill $T $M 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19953/ && curl -s -o /dev/null http://127.0.0.1:19954/ && break; sleep 0.1; done
		{shared.delta} compare -c {shared.ignore.yaml} --truth http://127.0.0.1:19953 --mirror local=http://127.0.0.1:19954 /repos/o/demo
		echo "exit=$?"
	  outputs:
		stdout:
			- "GET /repos/o/demo [local] truth=200 mirror=200: match (3 ignored)"
			- "exit=0"
		!stdout:
			- "differences:"

	- desc: a config error names every problem and exits 2
	  timeout: 10s
	  cmd: |
		{shared.delta} compare --truth notaurl --ignore 'x.y' /a
		echo "exit=$?"
	  outputs:
		stderr:
			- "truth: url"
			- "mirrors: at least one mirror is required"
			- "must start with $"
		stdout:
			- "exit=2"

	- desc: serve answers from the truth, compares in the background, and keeps writes off the mirror
	  timeout: 30s
	  cmd: |
		{shared.fixtureserver} -listen 127.0.0.1:19955 -route /repos/o/demo={shared.truth-demo.json} &
		T=$!
		{shared.fixtureserver} -listen 127.0.0.1:19956 -route /repos/o/demo={shared.mirror-demo.json} &
		M=$!
		{shared.delta} serve --listen 127.0.0.1:19957 --truth http://127.0.0.1:19955 --mirror local=http://127.0.0.1:19956 --report {outputs.delta.ndjson} &
		D=$!
		trap "kill $T $M $D 2>/dev/null" EXIT
		for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:19957/_delta/summary && curl -s -o /dev/null http://127.0.0.1:19955/ && curl -s -o /dev/null http://127.0.0.1:19956/ && break; sleep 0.1; done
		curl -s -D - http://127.0.0.1:19957/repos/o/demo
		echo
		curl -s -o /dev/null -X POST http://127.0.0.1:19957/repos/o/demo
		for i in $(seq 1 50); do curl -s http://127.0.0.1:19957/_delta/summary | grep -q '"exchanges": 1' && break; sleep 0.1; done
		curl -s 'http://127.0.0.1:19957/_delta/summary?format=text'
	  outputs:
		stdout:
			- "X-Delta-Served: truth"
			- '"stargazers_count": 5'
			- "exchanges 1  matched 0  differing 1  errors 0  uncompared 1"
			- "1x GET /repos/o/demo [local] changed $.stargazers_count"
		files:
			delta.ndjson:
				match:
					- '"path":"\$.stargazers_count"'
