module github.com/wow-look-at-my/api-mirror

go 1.25.0

// Pinned to a concrete commit until api-dsl's own branch merges. Tracking its
// default branch resolves to a commit that predates the package.
require github.com/wow-look-at-my/api-dsl v0.0.0-20260823131817-89a1743c497d // go-toolchain:auto-branch
