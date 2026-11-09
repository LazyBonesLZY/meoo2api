// Package webui 嵌入控制台静态资源。
package webui

import _ "embed"

//go:embed index.html
var index []byte

func Index() []byte { return index }
