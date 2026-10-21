package web

import "embed"

// Files contains the dashboard without a separate frontend build step.
//
//go:embed index.html app.js style.css
var Files embed.FS
