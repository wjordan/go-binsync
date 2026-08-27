package main

import _ "embed"

// The page is one file with no external requests: the demo measures
// transfers, so a stylesheet or a script arriving from somewhere else would
// be measuring that instead.
//
//go:embed index.html
var indexHTML []byte
