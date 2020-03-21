package main

import (
	"html/template"
	"log"
)

var (
	homeTemplate, moduleTemplate, buildTemplate *template.Template
)

func init() {
	var err error

	buildTemplate, err = template.New("build").Parse(buildTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing build html template: %v", err)
	}

	moduleTemplate, err = template.New("module").Parse(moduleTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing module html template: %v", err)
	}

	homeTemplate, err = template.New("home").Parse(homeTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing home html template: %v", err)
	}
}

const htmlTemplateString = `
{{ define "html" }}
<!doctype html>
<html>
	<head>
		<title>{{ template "title" . }}</title>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width">
		<style>
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 50rem; font-family: Ubuntu, Lato, sans-serif; color: #111; line-height: 1.3; }
body, input, button { font-size: 17px; }
h1 { font-size: 1.5rem; }
h2 { font-size: 1.25rem; }
ul { padding-left: 1rem; }
a { color: #007d9c; }
.buildlink { padding: 0 .2rem; display: inline-block; }
.buildlink.unsupported { color: #aaa; }
.buildlink.active { padding: .1rem .2rem; border-radius: .2rem; color: white; background-color: #007d9c; }
.buildlink.unsupported.active { color: white; background-color: #aaa; }
.success { color: #14ae14; }
.failure { color: #d34826; }
.output { margin-left: calc(-50vw + 25rem); width: calc(100vw - 2rem); }
		</style>
	</head>
	<body>
		<div style="margin:1rem">
{{ template "content" . }}
		</div>
	</body>
</html>
{{ end }}
{{ template "html" . }}
`

const buildTemplateString = `
{{ define "title" }}{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }} - {{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }} - gobuild{{ end }}
{{ define "content" }}
	<p><a href="/">&lt; Home</a></p>
	<h1>
		{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }}<br/>
		{{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}
		{{ if .Success}}<br/>{{ .Sum }}{{ end }}
		{{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}
	</h1>

{{ if .Success }}
	<h2>Download</h2>
	<table>
		<tr>
			<td><a href="{{ .DownloadFilename }}">{{ .DownloadFilename }}</a></td>
			<td style="padding-left: 1rem">{{ .Filesize }}</td>
		</tr>
		<tr>
			<td><a href="{{ .DownloadFilename }}.gz">{{ .DownloadFilename }}.gz</a></td>
			<td style="padding-left: 1rem">{{ .FilesizeGz }}</td>
		</tr>
	</table>

	<h2>More</h2>
	<ul>
		<li><a href="log">Build log</a></li>
		<li><a href="/x/{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/{{ .Req.Mod }}@latest/{{ .Req.Dir }}">{{ .Req.Goos }}-{{ .Req.Goarch }}-<b>latest</b>/{{ .Req.Mod }}@<b>latest</b>/{{ .Req.Dir }}</a> (<a href="/x/{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/{{ .Req.Mod }}@latest/{{ .Req.Dir }}dl">direct download</a>)</li>
		<li>Built on {{ .Start }}, in {{ .BuildWallTimeMS }}ms; sys {{ .SystemTimeMS }}ms, user {{ .UserTimeMS }}ms.</li>
	</ul>

	<h2>Links</h2>
	<ul>
		<li>Documentation at <a href="{{ .PkgGoDevURL }}">pkg.go.dev</a></li>
	</ul>

{{ else }}
	<h2>Error</h2>
	<div class="output">
		<pre>
{{ .Output }}
		</pre>
	</div>
{{ end }}

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Module versions</h2>
	{{ if .Mod.Err }}
		<div>error: {{ .Mod.Err }}</div>
	{{ else }}
	{{ range .Mod.VersionLinks }}	<div><a href="/x/{{ .Path }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Version }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
	{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Go versions</h2>
	{{ range .GoversionLinks }}	<div><a href="/x/{{ .Path }}" class="buildlink{{ if .Active }} active{{ end }} {{ if not .Supported }} unsupported{{ end }}">{{ .Goversion }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Targets</h2>
	{{ range .TargetLinks }}	<div><a href="/x/{{ .Path }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Goos }}/{{ .Goarch }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
	</div>
{{ end }}
`

const moduleTemplateString = `
{{ define "title" }}{{ .Module }}@{{ .Version }} - gobuild{{ end }}
{{ define "content" }}
	<p><a href="/">&lt; Home</a></p>
	<h1>{{ .Module }}@{{ .Version }}</h1>
	<p>Main packages:</p>
	<ul>
{{ range .Mains }}		<li><a href="{{ .Link }}">{{ .Name }}</a></li>{{ end }}
	</ul>
{{ end }}
`

const homeTemplateString = `
{{ define "title" }}reproducible binaries for the go module proxy - gobuild{{ end }}
{{ define "content" }}
		<h1>gobuild - reproducible binaries with the go module proxy</h1>
		<p>Gobuild deterministically compiles Go code available through the Go module proxy, and returns the binary.</p>

		<p>The <a href="https://proxy.golang.org/">Go module proxy</a> ensures source code stays available, and you are likely to get the same code each time you fetch it. Gobuild aims to achieve the same for binaries.</p>

		<h2>Try a module</h2>
		<form onsubmit="location.href = '/m/' + moduleName.value; return false" method="GET" action="/m/">
			<input id="moduleName" name="m" type="text" placeholder="github.com/your/project" style="width:30rem; max-width:75%" />
			<button type="submit">Go!</button>
		</form>

		<h2>URLs</h2>
		<p>Composition of URLs:</p>
		<blockquote style="color:#666">{{ .BaseURL }}x/<span style="color:#111">&lt;goos&gt;</span>-<span style="color:#111">&lt;goarch&gt;</span>-<span style="color:#111">&lt;goversion&gt;</span>/<span style="color:#111">&lt;module&gt;</span>@<span style="color:#111">&lt;version&gt;</span>/<span style="color:#111">&lt;package&gt;</span>/</blockquote>
		<p>Example:</p>
		<blockquote><a href="/x/linux-amd64-latest/github.com/mjl-/sherpa/@latest/cmd/sherpaclient/">{{ .BaseURL }}x/linux-amd64-latest/github.com/mjl-/sherpa/@latest/cmd/sherpaclient/</a></blockquote>
		<p>Opening this URL will either start a build, or show the results of an earlier build. The page shows links to download the binary, view the build output log file, the sha256 sum of the binary. You'll also see cross references to builds with different goversion, goos, goarch, and different versions of the module. You need not and cannot refresh a build, because they are reproducible.</p>

		<h2>Recent builds</h2>
		<ul>
{{ range .Recents }}			<li><a href="/x/{{ . }}">{{ . }}</a></li>{{ end }}
		</ul>

		<h2>More</h2>
		<p>Builds are created with CGO_ENABLED=0, -trimpath flag, and a zero buildid.</p>
		<p>Only "go build" is run. No "go test", "go generate", no build tags, no cgo, no custom compile/link flags, no makefiles, etc.</p>
		<p>Modules are looked up through the go proxy. That's why shorthand versions like "@v1" don't resolve.</p>
		<p>Code is available at <a href="https://github.com/mjl-/gobuild">github.com/mjl-/gobuild</a>, under MIT-license.</p>
{{ end }}
`
