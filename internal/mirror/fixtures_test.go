package mirror

import _ "embed"

// The fixtures below are files rather than string literals.

//go:embed absorb_project_doc.json
var absorbProjectDocJSON string

//go:embed absorb_trim_doc.json
var absorbTrimDocJSON string

//go:embed credential_test_spec.xml
var credentialTestSpecXML string

//go:embed gsm_insert.sql
var gsmInsertSQL string

//go:embed ops_spec.xml
var opsSpecDoc string

//go:embed ops_dashboard.xml
var opsDashboardXML string

//go:embed ops_ratelimit.xml
var opsRateLimitXML string

//go:embed ops_debounce.xml
var opsDebounceXML string

//go:embed ops_cors.xml
var opsCORSXML string

//go:embed ops_refresh.xml
var opsRefreshXML string

//go:embed ops_refresh_unknown_kind.xml
var opsRefreshUnknownKindXML string

//go:embed ops_replay.xml
var opsReplayXML string

//go:embed ops_replay_no_redeliver.xml
var opsReplayNoRedeliverXML string

//go:embed ops_replay_no_id.xml
var opsReplayNoIDXML string

//go:embed ops_notify_events.xml
var opsNotifyEventsXML string

//go:embed mirror_accept.xml
var mirrorAcceptXML string

//go:embed list_route_report.xml
var listRouteReportXML string

//go:embed no_events_report.xml
var noEventsReportXML string

//go:embed probe_defaults.xml
var probeDefaultsXML string

//go:embed meaningless_spec.xml
var meaninglessSpecXML string
