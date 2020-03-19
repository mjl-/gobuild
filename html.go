package main

import (
	"net/http"
)

const lead = `
<!doctype html>
<html>
	<head>
		<title>gobuild - reproducible binaries for the go module proxy</title>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width">
		<style>
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 50rem; font-family: Ubuntu, Lato, sans-serif; color: #111; line-height: 1.3; }
h1 { font-size: 1.75rem; }
h2 { font-size: 1.5rem; }
a { color: #007d9c; }
		</style>
	</head>
	<body>
		<div style="margin:1rem">
`

const trail = `		</div>
	</body>
</html>
`

func writeHTML(w http.ResponseWriter, content []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(lead))
	w.Write(content)
	w.Write([]byte(trail))
}
