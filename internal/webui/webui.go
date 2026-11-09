// Package webui 嵌入控制台静态资源。
package webui

import (
	"bytes"
	_ "embed"
)

//go:embed index.html
var index []byte

//go:embed console.js
var script []byte

// Index keeps the console a self-contained page without another static route.
// Return an independent slice so callers cannot mutate the embedded assets.
func Index() []byte {
	return bytes.Replace(index, []byte("/*__CONSOLE_SCRIPT__*/"), script, 1)
}
