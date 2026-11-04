package web

import "embed"

// Files contains the dashboard without a separate frontend build step.
//
//go:embed index.html slider.html app.js tasks.js style.css theme.js
var Files embed.FS
