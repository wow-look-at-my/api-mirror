package mirror

import (
	apidsl "github.com/wow-look-at-my/api-dsl"
)

// The spec language lives in api-dsl, shared with api-cli. These wrappers keep
var (
	renderString = apidsl.RenderString
	lookupPath   = apidsl.LookupPath
	isTruthy     = apidsl.IsTruthy
	envMap       = apidsl.EnvMap
)

// node is parsed XML element.
type node = apidsl.Node

// checkAttrs rejects an attribute the element does not declare, which is what
var checkAttrs = apidsl.CheckAttrs

var (
	parseDOM       = apidsl.ParseDOM
	compileContent = apidsl.CompileContent
	textOf         = apidsl.TextOf
)
