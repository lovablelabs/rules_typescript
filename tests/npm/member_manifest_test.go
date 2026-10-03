package npm_test

import (
	"testing"

	"github.com/mikn/rules_typescript/tests/verify"
)

func TestWorkspaceManifestsDoNotRetainSourceEntryPoints(t *testing.T) {
	tree := verify.New(t)
	tree.File("packages/shared/package.json").Contains(
		`"./src/index.js"`,
		`"./src/wire/index.js"`,
	)
	tree.File("packages/shared/shared.package.json").Contains(
		`"exports":{".":"./src/index.js","./wire":"./src/wire/index.js"}`,
		`"type":"module"`,
	)
	tree.File("tests/jsx_preserve/member/package.json").Contains(
		`"exports":"./view.jsx"`,
	)
	tree.File("tests/jsx_preserve/member/member.package.json").Contains(
		`"exports":"./view.jsx"`,
	)
}
