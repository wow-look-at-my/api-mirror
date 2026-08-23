package main

import (
	apidsl "github.com/wow-look-at-my/api-dsl"
)

// The spec language lives in api-dsl, shared with api-cli. These wrappers keep
// its names short at the call sites here and mark the boundary: everything
// below this line is the language, everything above it is what a mirror means.
var (
	renderString = apidsl.RenderString
	lookupPath   = apidsl.LookupPath
	isTruthy     = apidsl.IsTruthy
	envMap       = apidsl.EnvMap
)

// node is one parsed XML element.
type node = apidsl.Node

// checkAttrs rejects an attribute the element does not declare, which is what
// turns a typo into an error at load instead of a silently ignored setting.
var checkAttrs = apidsl.CheckAttrs

var (
	parseDOM        = apidsl.ParseDOM
	compileContent  = apidsl.CompileContent
	compileTextElem = apidsl.CompileTextElem
	textOf          = apidsl.TextOf
)
